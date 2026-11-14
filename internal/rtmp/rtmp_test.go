package rtmp

import (
	"net"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/flv"
	"github.com/AlexxIT/go2rtc/pkg/rtmp"
	"github.com/stretchr/testify/require"
)

// A saved config moves the RTMP server to another port or turns it off, without
// a restart of go2rtc
func TestListenerOfSavedConfig(t *testing.T) {
	t.Cleanup(func() { listen("") })

	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		return ln.Addr().String()
	}
	dial := func(addr string) error {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	first, second := free(), free()

	reloadConfig([]byte("rtmp:\n  listen: " + first + "\n"))
	require.NoError(t, dial(first))

	reloadConfig([]byte("rtmp:\n  listen: " + second + "\n"))
	require.NoError(t, dial(second))
	require.Error(t, dial(first))

	// a config that is no YAML for the module changes nothing
	reloadConfig([]byte("rtmp:\n  listen: [1, 2]\n"))
	require.NoError(t, dial(second))

	reloadConfig([]byte("streams: {}\n"))
	require.Error(t, dial(second))
}

// With streams in the config the server plays and takes only those, a saved
// config changes the list without a restart
func TestServerOffersOnlyTheStreamsOfTheConfig(t *testing.T) {
	t.Cleanup(func() { setOffered(nil) })

	for _, name := range []string{"cam_open", "cam_closed"} {
		_, err := streams.New(name)
		require.NoError(t, err)
		t.Cleanup(func() { streams.Delete(name) })
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	url := "rtmp://" + ln.Addr().String() + "/"

	// what the server made of one connection, cut after a moment: a stream it
	// offers has no source here and would wait for one
	serve := func(dial func()) error {
		go dial()
		conn, err := ln.Accept()
		require.NoError(t, err)
		cut := time.AfterFunc(300*time.Millisecond, func() { _ = conn.Close() })
		defer cut.Stop()
		return tcpHandle(conn)
	}
	play := func(name string) error {
		return serve(func() { _, _ = rtmp.DialPlay(url + name) })
	}
	publish := func(name string) error {
		return serve(func() { _, _ = rtmp.DialPublish(url+name, flv.NewConsumer()) })
	}
	notFound := func(name string) string { return "stream not found: " + name }

	// no list: every stream, as before
	require.NotEqual(t, notFound("cam_closed"), errText(play("cam_closed")))

	reloadConfig([]byte("rtmp:\n  streams: [cam_open]\n"))
	require.NotEqual(t, notFound("cam_open"), errText(play("cam_open")))
	require.EqualError(t, play("cam_closed"), notFound("cam_closed"))
	require.EqualError(t, publish("cam_closed"), notFound("cam_closed"))
	require.NotEqual(t, notFound("cam_open"), errText(publish("cam_open")))

	reloadConfig([]byte("rtmp:\n  streams: []\n"))
	require.EqualError(t, play("cam_open"), notFound("cam_open"))

	reloadConfig([]byte("rtmp:\n  listen: \"\"\n"))
	require.NotEqual(t, notFound("cam_closed"), errText(play("cam_closed")))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
