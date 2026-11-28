package tcp

import (
	"bufio"
	"bytes"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestName(t *testing.T) {
	data := []byte(`RTSP/1.0 401 Unauthorized
WWW-Authenticate: Digest realm="testrealm@host.com",
                        nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093",

`)

	buf := bytes.NewBuffer(data)
	r := bufio.NewReader(buf)

	res, err := ReadResponse(r)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}

func TestRequestStringHeaderOrder(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	require.NoError(t, err)

	req := &Request{
		Method: "DESCRIBE",
		URL:    u,
		Proto:  "RTSP/1.0",
		Header: textproto.MIMEHeader{
			"User-Agent":     {"go2rtc"},
			"Accept":         {"application/sdp"},
			"CSeq":           {"2"},
			"Authorization":  {"Basic dGVzdA=="},
			"Content-Length": {"0"},
		},
	}

	expected := "DESCRIBE rtsp://example.com/stream RTSP/1.0\r\n" +
		"CSeq: 2\r\n" +
		"Authorization: Basic dGVzdA==\r\n" +
		"User-Agent: go2rtc\r\n" +
		"Accept: application/sdp\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"

	require.Equal(t, expected, req.String())
}

func TestResponseStringHeaderOrder(t *testing.T) {
	res := &Response{
		Proto:  "RTSP/1.0",
		Status: "200 OK",
		Header: textproto.MIMEHeader{
			"Public":         {"OPTIONS, DESCRIBE"},
			"Content-Length": {"0"},
			"CSeq":           {"3"},
		},
	}

	expected := "RTSP/1.0 200 OK\r\n" +
		"CSeq: 3\r\n" +
		"Public: OPTIONS, DESCRIBE\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"

	require.Equal(t, expected, res.String())
}

func TestWriteHeaders(t *testing.T) {
	tests := []struct {
		name     string
		header   textproto.MIMEHeader
		expected string
	}{
		{
			name: "nil headers",
		},
		{
			name:   "empty headers",
			header: textproto.MIMEHeader{},
		},
		{
			name: "without CSeq",
			header: textproto.MIMEHeader{
				"User-Agent": {"go2rtc"},
				"Accept":     {"application/sdp"},
			},
			expected: "User-Agent: go2rtc\r\n" +
				"Accept: application/sdp\r\n",
		},
		{
			name: "empty values",
			header: textproto.MIMEHeader{
				"CSeq":           nil,
				"Accept":         {},
				"User-Agent":     {"go2rtc"},
				"Content-Length": nil,
			},
			expected: "User-Agent: go2rtc\r\n",
		},
		{
			name: "multiple values",
			header: textproto.MIMEHeader{
				"CSeq":   {"1"},
				"Accept": {"application/sdp", "application/json"},
			},
			expected: "CSeq: 1\r\n" +
				"Accept: application/sdp\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s strings.Builder
			writeHeaders(&s, tt.header)
			require.Equal(t, tt.expected, s.String())
		})
	}
}

func TestWriteHeadersOrder(t *testing.T) {
	header := textproto.MIMEHeader{
		"CSeq":             {"1"},
		"Authorization":    {"Basic dGVzdA=="},
		"User-Agent":       {"go2rtc"},
		"Accept":           {"application/sdp"},
		"Content-Type":     {"application/sdp"},
		"Range":            {"npt=0.000-"},
		"Transport":        {"RTP/AVP/TCP"},
		"Require":          {"www.onvif.org/ver20/backchannel"},
		"Session":          {"12345"},
		"Server":           {"go2rtc"},
		"Public":           {"OPTIONS, DESCRIBE"},
		"Www-Authenticate": {`Basic realm="go2rtc"`},
		"Content-Length":   {"0"},
	}

	expected := "CSeq: 1\r\n" +
		"Authorization: Basic dGVzdA==\r\n" +
		"User-Agent: go2rtc\r\n" +
		"Accept: application/sdp\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Range: npt=0.000-\r\n" +
		"Transport: RTP/AVP/TCP\r\n" +
		"Require: www.onvif.org/ver20/backchannel\r\n" +
		"Session: 12345\r\n" +
		"Server: go2rtc\r\n" +
		"Public: OPTIONS, DESCRIBE\r\n" +
		"Www-Authenticate: Basic realm=\"go2rtc\"\r\n" +
		"Content-Length: 0\r\n"

	var s strings.Builder
	writeHeaders(&s, header)
	require.Equal(t, expected, s.String())
}

func TestStringBody(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	require.NoError(t, err)

	header := textproto.MIMEHeader{
		"CSeq":           {"1"},
		"Content-Length": {"4"},
	}

	req := &Request{
		Method: "SET_PARAMETER",
		URL:    u,
		Proto:  "RTSP/1.0",
		Header: header,
		Body:   []byte("test"),
	}

	expected := "SET_PARAMETER rtsp://example.com/stream RTSP/1.0\r\n" +
		"CSeq: 1\r\n" +
		"Content-Length: 4\r\n" +
		"\r\n" +
		"test"

	require.Equal(t, expected, req.String())

	res := &Response{
		Proto:  "RTSP/1.0",
		Status: "200 OK",
		Header: header,
		Body:   []byte("test"),
	}

	expected = "RTSP/1.0 200 OK\r\n" +
		"CSeq: 1\r\n" +
		"Content-Length: 4\r\n" +
		"\r\n" +
		"test"

	require.Equal(t, expected, res.String())
}

// The map order is random, one serialization proves nothing.
func TestRequestStringCSeqFirst(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	require.NoError(t, err)

	for range 50 {
		req := &Request{
			Method: "DESCRIBE",
			URL:    u,
			Proto:  "RTSP/1.0",
			Header: map[string][]string{
				"User-Agent": {"go2rtc"},
				"Accept":     {"application/sdp"},
				"Session":    {"12345"},
				"CSeq":       {"2"},
			},
		}

		expected := "DESCRIBE rtsp://example.com/stream RTSP/1.0\r\n" +
			"CSeq: 2\r\n" +
			"User-Agent: go2rtc\r\n" +
			"Accept: application/sdp\r\n" +
			"Session: 12345\r\n" +
			"\r\n"
		if s := req.String(); s != expected {
			t.Fatalf("unexpected request:\n%q\nwant:\n%q", s, expected)
		}
	}
}

// Header.Set stores the key as Cseq, it has to go first all the same: the
// order of upstream only knows CSeq and would drop it.
func TestResponseStringCSeqFirst(t *testing.T) {
	for range 50 {
		res := &Response{
			Proto:  "RTSP/1.0",
			Status: "200 OK",
			Header: map[string][]string{},
		}
		res.Header.Set("Session", "12345")
		res.Header.Set("Public", "OPTIONS, DESCRIBE")
		res.Header.Set("Content-Length", "0")
		res.Header.Set("CSeq", "3")

		expected := "RTSP/1.0 200 OK\r\n" +
			"Cseq: 3\r\n" +
			"Session: 12345\r\n" +
			"Public: OPTIONS, DESCRIBE\r\n" +
			"Content-Length: 0\r\n" +
			"\r\n"
		if s := res.String(); s != expected {
			t.Fatalf("unexpected response:\n%q\nwant:\n%q", s, expected)
		}
	}
}

// Headers outside the LIVE555 order are written after it, sorted: the order
// must not lose a header (ex. Content-Base or RTP-Info of a server).
func TestWriteHeadersKeepsUnknownHeaders(t *testing.T) {
	for range 50 {
		header := textproto.MIMEHeader{
			"Rtp-Info":       {"url=rtsp://x/trackID=0;seq=1"},
			"Content-Length": {"0"},
			"Content-Base":   {"rtsp://x/"},
			"CSeq":           {"4"},
			"Session":        {"12345"},
		}

		expected := "CSeq: 4\r\n" +
			"Session: 12345\r\n" +
			"Content-Length: 0\r\n" +
			"Content-Base: rtsp://x/\r\n" +
			"Rtp-Info: url=rtsp://x/trackID=0;seq=1\r\n"

		var s strings.Builder
		writeHeaders(&s, header)
		require.Equal(t, expected, s.String())
	}
}

func TestStringSkipsHeaderWithoutValue(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	require.NoError(t, err)

	header := map[string][]string{"CSeq": {}, "Session": {}, "Accept": {"application/sdp"}}

	req := &Request{Method: "DESCRIBE", URL: u, Proto: "RTSP/1.0", Header: header}
	require.Equal(t, "DESCRIBE rtsp://example.com/stream RTSP/1.0\r\nAccept: application/sdp\r\n\r\n", req.String())

	res := Response{Proto: "RTSP/1.0", Status: "200 OK", Header: header}
	require.Equal(t, "RTSP/1.0 200 OK\r\nAccept: application/sdp\r\n\r\n", res.String())
}

func TestContentLengthBounds(t *testing.T) {
	for _, length := range []string{"-1", "1048577", "9223372036854775807"} {
		req := "OPTIONS rtsp://x/ RTSP/1.0\r\nCSeq: 1\r\nContent-Length: " + length + "\r\n\r\n"
		if _, err := ReadRequest(bufio.NewReader(bytes.NewBufferString(req))); err == nil {
			t.Fatalf("request with Content-Length %s accepted", length)
		}

		res := "RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: " + length + "\r\n\r\n"
		if _, err := ReadResponse(bufio.NewReader(bytes.NewBufferString(res))); err == nil {
			t.Fatalf("response with Content-Length %s accepted", length)
		}
	}

	req, err := ReadRequest(bufio.NewReader(bytes.NewBufferString("ANNOUNCE rtsp://x/ RTSP/1.0\r\nCSeq: 1\r\nContent-Length: 4\r\n\r\nv=0\n")))
	require.NoError(t, err)
	require.Equal(t, "v=0\n", string(req.Body))
}

func FuzzReadRequest(f *testing.F) {
	f.Add([]byte("OPTIONS rtsp://x/ RTSP/1.0\r\nCSeq: 1\r\nContent-Length: -1\r\n\r\n"))
	f.Add([]byte("ANNOUNCE rtsp://x/ RTSP/1.0\r\nCSeq: 1\r\nContent-Length: 4\r\n\r\nv=0\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadRequest(bufio.NewReader(bytes.NewReader(data)))
	})
}

func FuzzReadResponse(f *testing.F) {
	f.Add([]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 4\r\n\r\nv=0\n"))
	f.Add([]byte("RTSP/1.0 401 Unauthorized\r\nWWW-Authenticate: Digest realm=\"a\",\r\n  nonce=\"b\"\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ReadResponse(bufio.NewReader(bytes.NewReader(data)))
	})
}
