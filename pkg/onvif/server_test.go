package onvif

import (
	"bytes"
	"encoding/xml"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// streamingCapabilities reads RTP_TCP and RTP_RTSP_TCP of a response, as
// elements (GetCapabilities) or as attributes (GetServiceCapabilities)
func streamingCapabilities(t *testing.T, b []byte) map[string]string {
	t.Helper()
	caps := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(b))
	var element string
	for {
		token, err := d.Token()
		if err == io.EOF {
			return caps
		}
		require.NoError(t, err, "%s", b)
		switch token := token.(type) {
		case xml.StartElement:
			element = token.Name.Local
			for _, attr := range token.Attr {
				if attr.Name.Local == "RTP_TCP" || attr.Name.Local == "RTP_RTSP_TCP" {
					caps[attr.Name.Local] = attr.Value
				}
			}
		case xml.CharData:
			if element == "RTP_TCP" || element == "RTP_RTSP_TCP" {
				caps[element] = string(bytes.TrimSpace(token))
			}
		case xml.EndElement:
			element = ""
		}
	}
}

// Clients such as Hikvision ISC check RTP_TCP before they set up a stream,
// with false they skipped it. The RTSP server streams over TCP interleaved,
// what cameras declare as RTP_TCP too.
func TestStreamingOverTCP(t *testing.T) {
	want := map[string]string{"RTP_TCP": "true", "RTP_RTSP_TCP": "true"}
	require.Equal(t, want, streamingCapabilities(t, GetCapabilitiesResponse("http", "192.168.1.2:8899")))
	require.Equal(t, want, streamingCapabilities(t, StaticResponse(ServiceGetServiceCapabilities)))
}
