package dvrip

import (
	"bufio"
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// readJSON feeds one DVRIP chunk (20-byte header, then the payload) to a
// client and returns what ReadJSON makes of it.
func readJSON(t *testing.T, payload []byte) (res Response, err error) {
	t.Helper()

	client, camera := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = camera.Close()
	})

	go func() {
		hdr := make([]byte, 20)
		hdr[0] = 255
		binary.LittleEndian.PutUint32(hdr[16:], uint32(len(payload)))
		_, _ = camera.Write(append(hdr, payload...))
	}()

	c := &Client{conn: client, rd: bufio.NewReader(client)}
	require.NotPanics(t, func() { res, err = c.ReadJSON() })
	return
}

func TestReadJSONTerminators(t *testing.T) {
	const body = `{"Ret":100,"SessionID":"0x0000003c"}`

	// the usual "\n\x00" ending
	res, err := readJSON(t, []byte(body+"\x0a\x00"))
	require.NoError(t, err)
	require.Equal(t, "0x0000003c", res["SessionID"])

	// Based on AlexxIT/go2rtc#2520: some iCSee firmware ends with "\x00"
	// alone, cutting two bytes used to drop the closing brace
	res, err = readJSON(t, []byte(body+"\x00"))
	require.NoError(t, err)
	require.Equal(t, "0x0000003c", res["SessionID"])
}

// A camera announcing a chunk shorter than the terminator must fail the
// request, not slice with a negative index and take down the process.
func TestReadJSONShortPayload(t *testing.T) {
	for _, payload := range [][]byte{{}, {0}} {
		_, err := readJSON(t, payload)
		require.Error(t, err)
	}
}

func TestReadJSONWrongRet(t *testing.T) {
	_, err := readJSON(t, []byte(`{"Ret":205}`+"\x0a\x00"))
	require.ErrorContains(t, err, "wrong response")
}
