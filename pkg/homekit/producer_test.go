package homekit

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/hap"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
	"github.com/AlexxIT/go2rtc/pkg/srtp"
	"github.com/stretchr/testify/require"
)

// testCamera is a paired HomeKit camera with a single stream slot. As some
// real ones do, it keeps the slot reserved after the HAP connection closes,
// until the stream is ended.
type testCamera struct {
	acc  *hap.Accessory
	addr string

	devicePrivate, clientPrivate []byte

	failEndpoints bool          // reading the endpoints fails, after the slot is reserved
	failStart     bool          // the start command fails
	hold          chan struct{} // the held command is answered once released
	holdCommand   byte
	release       func()

	started  chan struct{} // the start command arrived
	setUp    chan struct{} // the stream config is read back, the last step of the setup
	reserved atomic.Bool
	requests atomic.Int32
	ends     atomic.Int32
}

func holdStart(cam *testCamera) {
	cam.hold = make(chan struct{})
	cam.holdCommand = camera.SessionCommandStart
	cam.release = sync.OnceFunc(func() { close(cam.hold) })
}

func holdEnd(cam *testCamera) {
	holdStart(cam)
	cam.holdCommand = camera.SessionCommandEnd
}

func newTestCamera(t *testing.T, options ...func(*testCamera)) *testCamera {
	t.Helper()
	_, devicePrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, clientPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	cam := &testCamera{
		acc:           camera.NewAccessory("test", "camera", "test", "1", "1"),
		addr:          ln.Addr().String(),
		devicePrivate: devicePrivate,
		clientPrivate: clientPrivate,
		started:       make(chan struct{}, 1),
		setUp:         make(chan struct{}, 1),
	}
	for _, option := range options {
		option(cam)
	}
	if cam.release != nil {
		t.Cleanup(cam.release)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go cam.serve(conn)
		}
	}()
	return cam
}

func (cam *testCamera) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	req, err := http.ReadRequest(rw.Reader)
	if err != nil {
		return
	}
	server := &hap.Server{DeviceID: "test-camera", DevicePrivate: cam.devicePrivate}
	_, key, err := server.PairVerify(req, rw)
	if err != nil {
		return
	}
	secure, err := hap.NewConn(conn, rw, key, false)
	if err != nil {
		return
	}

	reader := bufio.NewReader(secure)
	writer := bufio.NewWriter(secure)
	for {
		req, err = http.ReadRequest(reader)
		if err != nil {
			return
		}
		cam.requests.Add(1)
		code, body := cam.answer(req)
		if err = hap.WriteResponse(writer, code, hap.MimeJSON, body); err != nil {
			return
		}
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (cam *testCamera) answer(req *http.Request) (int, []byte) {
	service := cam.acc.GetService("110")
	status := service.GetCharacter(camera.TypeStreamingStatus)
	setup := service.GetCharacter(camera.TypeSetupEndpoints)
	selected := service.GetCharacter(camera.TypeSelectedStreamConfiguration)

	switch {
	case req.URL.Path == hap.PathAccessories:
		body, _ := json.Marshal(hap.JSONAccessories{Value: []*hap.Accessory{cam.acc}})
		return http.StatusOK, body

	case req.URL.Path == hap.PathCharacteristics && req.Method == "GET":
		var iid uint64
		_, _ = fmt.Sscanf(req.URL.Query().Get("id"), "1.%d", &iid)
		char := cam.acc.GetCharacterByID(iid)
		switch {
		case char == setup && cam.failEndpoints:
			return http.StatusInternalServerError, nil
		case char == selected:
			signal(cam.setUp)
		}
		body, _ := json.Marshal(hap.JSONCharacters{Value: []hap.JSONCharacter{{AID: 1, IID: iid, Value: char.Value}}})
		return http.StatusOK, body

	case req.URL.Path == hap.PathCharacteristics && req.Method == "PUT":
		var v hap.JSONCharacters
		if json.NewDecoder(req.Body).Decode(&v) != nil {
			return http.StatusBadRequest, nil
		}
		for _, v := range v.Value {
			char := cam.acc.GetCharacterByID(v.IID)
			char.Value = v.Value

			switch char {
			case setup:
				var offer camera.SetupEndpointsRequest
				_ = char.ReadTLV8(&offer)
				cam.reserved.Store(true)
				_ = status.Write(camera.StreamingStatus{Status: camera.StreamingStatusInUse})
				_ = setup.Write(camera.SetupEndpointsResponse{
					SessionID: offer.SessionID, Address: offer.Address,
					VideoCrypto: offer.VideoCrypto, AudioCrypto: offer.AudioCrypto, VideoSSRC: 1, AudioSSRC: 2,
				})

			case selected:
				var config camera.SelectedStreamConfiguration
				_ = char.ReadTLV8(&config)
				if cam.hold != nil && config.Control.Command == cam.holdCommand {
					if config.Control.Command == camera.SessionCommandStart {
						signal(cam.started)
					}
					<-cam.hold
				}
				switch config.Control.Command {
				case camera.SessionCommandEnd:
					cam.ends.Add(1)
					cam.reserved.Store(false)
					_ = status.Write(camera.StreamingStatus{Status: camera.StreamingStatusAvailable})
				case camera.SessionCommandStart:
					signal(cam.started)
					if cam.failStart {
						return http.StatusInternalServerError, nil
					}
				}
			}
		}
		return http.StatusNoContent, nil
	}

	return http.StatusNotFound, nil
}

// dial connects a producer to the camera, with a video track as a consumer
// asks for it
func (cam *testCamera) dial(t *testing.T) *Client {
	t.Helper()
	h := &hap.Client{
		DeviceAddress: cam.addr, DeviceID: "test-camera", DevicePublic: cam.devicePrivate[32:],
		ClientID: "test-client", ClientPrivate: cam.clientPrivate,
	}
	require.NoError(t, h.Connect())

	c := &Client{Connection: core.Connection{Transport: h}, hap: h, srtp: srtp.NewServer("127.0.0.1:0")}
	t.Cleanup(func() { _ = c.Stop() })

	medias := c.GetMedias()
	require.NotEmpty(t, medias)
	_, err := c.GetTrack(medias[0], medias[0].Codecs[0])
	require.NoError(t, err)
	return c
}

func start(c *Client) chan error {
	errs := make(chan error, 1)
	go func() { errs <- c.Start() }()
	return errs
}

func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: timeout", what)
		panic("unreachable")
	}
}

func shorten(t *testing.T, timeout *time.Duration) {
	old := *timeout
	*timeout = 200 * time.Millisecond
	t.Cleanup(func() { *timeout = old })
}

// A camera that keeps its slot after the connection closes has none left
// after a few reconnects: Stop ends the stream first, once. Start returns
// with it, a stopped producer doesn't start again.
func TestStopEndsTheStream(t *testing.T) {
	cam := newTestCamera(t)
	c := cam.dial(t)
	started := start(c)
	wait(t, cam.setUp, "setup")
	require.True(t, cam.reserved.Load())

	require.NoError(t, c.Stop())
	require.False(t, cam.reserved.Load())
	require.Equal(t, int32(1), cam.ends.Load())
	require.NoError(t, wait(t, started, "start"))

	require.NoError(t, c.Stop())
	require.Equal(t, int32(1), cam.ends.Load())

	requests := cam.requests.Load()
	require.Error(t, c.Start())
	require.Equal(t, requests, cam.requests.Load())
}

// A setup that fails after the camera reserved the slot ends the stream.
func TestFailedStartEndsTheStream(t *testing.T) {
	for name, option := range map[string]func(*testCamera){
		"endpoints": func(cam *testCamera) { cam.failEndpoints = true },
		"start":     func(cam *testCamera) { cam.failStart = true },
	} {
		t.Run(name, func(t *testing.T) {
			cam := newTestCamera(t, option)
			c := cam.dial(t)
			require.Error(t, wait(t, start(c), "start"))
			require.False(t, cam.reserved.Load())
			require.Equal(t, int32(1), cam.ends.Load())
		})
	}
}

// Stop during the setup waits for it, then ends the stream: in between, both
// would talk over the one HAP connection and the stream they end isn't
// there yet.
func TestStopWaitsForTheStreamSetup(t *testing.T) {
	cam := newTestCamera(t, holdStart)
	c := cam.dial(t)
	started := start(c)
	wait(t, cam.started, "start command")

	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop() }()
	select {
	case <-stopped:
		t.Fatal("stopped during the setup")
	case <-time.After(100 * time.Millisecond):
	}

	cam.release()
	require.NoError(t, wait(t, stopped, "stop"))
	require.False(t, cam.reserved.Load())
	require.Equal(t, int32(1), cam.ends.Load())
	require.NoError(t, wait(t, started, "start"))
}

// A stream without data ends when Start gives up on it: the producer worker
// closes the connection before it stops the producer, an end in Stop would
// come too late. Stop doesn't end it again.
func TestStartEndsAStreamWithoutData(t *testing.T) {
	shorten(t, &dataTimeout)
	for _, interrupt := range []bool{true, false} {
		t.Run(fmt.Sprint("interrupt=", interrupt), func(t *testing.T) {
			cam := newTestCamera(t)
			c := cam.dial(t)
			require.NoError(t, wait(t, start(c), "start"))
			require.False(t, cam.reserved.Load())
			require.Equal(t, int32(1), cam.ends.Load())

			if interrupt {
				require.NoError(t, c.Interrupt())
			}
			_ = c.Stop()
			require.Equal(t, int32(1), cam.ends.Load())
		})
	}
}

// A camera that stops answering can't hold Start, nor Stop and the stream that
// waits on it.
func TestSilentCameraHoldsNothing(t *testing.T) {
	shorten(t, &streamTimeout)

	t.Run("setup", func(t *testing.T) {
		cam := newTestCamera(t, holdStart)
		c := cam.dial(t)
		require.Error(t, wait(t, start(c), "start"))

		stopped := make(chan error, 1)
		go func() { stopped <- c.Stop() }()
		require.NoError(t, wait(t, stopped, "stop"))
	})

	t.Run("end", func(t *testing.T) {
		cam := newTestCamera(t, holdEnd)
		c := cam.dial(t)
		started := start(c)
		wait(t, cam.setUp, "setup")

		stopped := make(chan error, 1)
		go func() { stopped <- c.Stop() }()
		require.Error(t, wait(t, stopped, "stop"))
		require.NoError(t, wait(t, started, "start"))
	})
}
