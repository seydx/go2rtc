package tcp

import (
	"bufio"
	"bytes"
	"net/http"
	"net/url"
	"testing"
)

func assert(t *testing.T, one, two any) {
	if one != two {
		t.FailNow()
	}
}

func TestName(t *testing.T) {
	data := []byte(`RTSP/1.0 401 Unauthorized
WWW-Authenticate: Digest realm="testrealm@host.com",
                        nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093",

`)

	buf := bytes.NewBuffer(data)
	r := bufio.NewReader(buf)

	res, err := ReadResponse(r)
	assert(t, err, nil)

	assert(t, res.StatusCode, http.StatusUnauthorized)
}

// The map order is random, one serialization proves nothing.
func TestRequestStringCSeqFirst(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	assert(t, err, nil)

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
			"Accept: application/sdp\r\n" +
			"Session: 12345\r\n" +
			"User-Agent: go2rtc\r\n" +
			"\r\n"
		if s := req.String(); s != expected {
			t.Fatalf("unexpected request:\n%q\nwant:\n%q", s, expected)
		}
	}
}

// Header.Set stores the key as Cseq, it has to go first all the same: sorted
// on its own it would land behind Content-Length.
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
			"Content-Length: 0\r\n" +
			"Public: OPTIONS, DESCRIBE\r\n" +
			"Session: 12345\r\n" +
			"\r\n"
		if s := res.String(); s != expected {
			t.Fatalf("unexpected response:\n%q\nwant:\n%q", s, expected)
		}
	}
}

func TestStringSkipsHeaderWithoutValue(t *testing.T) {
	u, err := url.Parse("rtsp://example.com/stream")
	assert(t, err, nil)

	header := map[string][]string{"CSeq": {}, "Session": {}, "Accept": {"application/sdp"}}

	req := &Request{Method: "DESCRIBE", URL: u, Proto: "RTSP/1.0", Header: header}
	assert(t, req.String(), "DESCRIBE rtsp://example.com/stream RTSP/1.0\r\nAccept: application/sdp\r\n\r\n")

	res := Response{Proto: "RTSP/1.0", Status: "200 OK", Header: header}
	assert(t, res.String(), "RTSP/1.0 200 OK\r\nAccept: application/sdp\r\n\r\n")
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
	assert(t, err, nil)
	assert(t, string(req.Body), "v=0\n")
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
