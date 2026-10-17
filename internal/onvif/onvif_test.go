package onvif

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/rtsp"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/tcp"
	"github.com/stretchr/testify/require"
)

var names = []string{
	"camera",
	"Front Door",
	"nest_enterprises%2Fproject%2Fdevices%2Fdevice-camera",
	"garage/main",
	"Tom & Jerry",
	"x<y>",
	`say "hi"`,
	"100%",
	"what?",
}

func useStreams(t *testing.T) {
	port := rtsp.Port
	rtsp.Port = "8554"
	t.Cleanup(func() { rtsp.Port = port })

	for _, name := range names {
		_, err := streams.New(name)
		require.NoError(t, err)
		t.Cleanup(func() { streams.Delete(name) })
	}
}

// request sends an operation of the media service to the ONVIF server
func request(t *testing.T, operation, body string) []byte {
	t.Helper()
	return requestHost(t, "192.168.1.2:1984", operation, body)
}

func requestHost(t *testing.T, host, operation, body string) []byte {
	t.Helper()
	req := httptest.NewRequest("POST", "/onvif/media_service", strings.NewReader(
		`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>`+
			`<trt:`+operation+` xmlns:trt="http://www.onvif.org/ver10/media/wsdl">`+body+`</trt:`+operation+`>`+
			`</s:Body></s:Envelope>`,
	))
	req.Host = host
	w := httptest.NewRecorder()
	onvifDeviceService(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.Bytes()
}

// response is a response read the way a client reads it: the token attributes
// and the texts of its elements, by element name. Invalid XML fails the test.
type response struct {
	tokens, texts map[string][]string
}

func parse(t *testing.T, b []byte) response {
	t.Helper()
	res := response{tokens: map[string][]string{}, texts: map[string][]string{}}
	var stack []string
	var text strings.Builder
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		token, err := d.Token()
		if err == io.EOF {
			return res
		}
		require.NoError(t, err, "%s", b)
		switch token := token.(type) {
		case xml.StartElement:
			stack = append(stack, token.Name.Local)
			text.Reset()
			for _, attr := range token.Attr {
				if attr.Name.Local == "token" {
					res.tokens[token.Name.Local] = append(res.tokens[token.Name.Local], attr.Value)
				}
			}
		case xml.CharData:
			text.Write(token)
		case xml.EndElement:
			if s := strings.TrimSpace(text.String()); s != "" {
				res.texts[stack[len(stack)-1]] = append(res.texts[stack[len(stack)-1]], s)
			}
			text.Reset()
			stack = stack[:len(stack)-1]
		}
	}
}

// A stream name is any text, the ONVIF server put it into its XML as it was.
// A single name with & or < made the profiles of all streams invalid XML: the
// client saw no camera at all.
func TestProfilesOfAnyName(t *testing.T) {
	useStreams(t)

	res := parse(t, request(t, "GetProfiles", ""))
	require.ElementsMatch(t, names, res.tokens["Profiles"])
	require.ElementsMatch(t, names, res.tokens["VideoSourceConfiguration"])
	require.ElementsMatch(t, names, res.texts["SourceToken"])

	res = parse(t, request(t, "GetVideoSources", ""))
	require.ElementsMatch(t, names, res.tokens["VideoSources"])

	res = parse(t, request(t, "GetVideoSourceConfigurations", ""))
	require.ElementsMatch(t, names, res.tokens["Configurations"])
}

// The client sends the token back, escaped for XML. The URIs of the stream
// had it as it was: a name with %2F, a space or another reserved character
// asked the RTSP server for a stream of another name, or broke the URI.
func TestURIsOfAnyName(t *testing.T) {
	useStreams(t)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			token := html.EscapeString(name)

			res := parse(t, request(t, "GetProfile", "<trt:ProfileToken>"+token+"</trt:ProfileToken>"))
			require.Equal(t, []string{name}, res.tokens["Profile"])

			res = parse(t, request(t, "GetVideoSourceConfiguration", "<trt:ConfigurationToken>"+token+"</trt:ConfigurationToken>"))
			require.Equal(t, []string{name}, res.tokens["Configuration"])

			res = parse(t, request(t, "GetStreamUri", "<trt:ProfileToken>"+token+"</trt:ProfileToken>"))
			require.Len(t, res.texts["Uri"], 1)
			uri := res.texts["Uri"][0]
			require.True(t, strings.HasPrefix(uri, "rtsp://192.168.1.2:8554/"), uri)

			// what the RTSP server reads from the request of the client
			req, err := tcp.ReadRequest(bufio.NewReader(strings.NewReader("DESCRIBE " + uri + " RTSP/1.0\r\nCSeq: 1\r\n\r\n")))
			require.NoError(t, err)
			require.Equal(t, name, req.URL.Path[1:])

			res = parse(t, request(t, "GetSnapshotUri", "<trt:ProfileToken>"+token+"</trt:ProfileToken>"))
			require.Len(t, res.texts["Uri"], 1)
			u, err := url.Parse(res.texts["Uri"][0])
			require.NoError(t, err)
			require.Equal(t, "http://192.168.1.2:1984/api/frame.jpeg", u.Scheme+"://"+u.Host+u.Path)
			require.Equal(t, name, u.Query().Get("src"))
		})
	}
}

// The URIs have the host the client asked for, which is any text too.
func TestURIsOfAnyHost(t *testing.T) {
	useStreams(t)

	res := parse(t, requestHost(t, "cam&era:1984", "GetStreamUri", "<trt:ProfileToken>camera</trt:ProfileToken>"))
	require.Equal(t, []string{"rtsp://cam&era:8554/camera"}, res.texts["Uri"])

	res = parse(t, requestHost(t, "cam&era:1984", "GetSnapshotUri", "<trt:ProfileToken>camera</trt:ProfileToken>"))
	require.Equal(t, []string{"http://cam&era:1984/api/frame.jpeg?src=camera"}, res.texts["Uri"])
}
