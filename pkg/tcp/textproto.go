package tcp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const EndLine = "\r\n"

// bodies are SDP or parameter lists, the limit only stops a peer from
// making us allocate what it names in Content-Length
const maxBodySize = 1 << 20

func readBody(r *bufio.Reader, header textproto.MIMEHeader) ([]byte, error) {
	val := header.Get("Content-Length")
	if val == "" {
		return nil, nil
	}

	size, err := strconv.Atoi(val)
	if err != nil {
		return nil, err
	}
	if size < 0 || size > maxBodySize {
		return nil, fmt.Errorf("wrong content length: %s", val)
	}

	body := make([]byte, size)
	if _, err = io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// Response like http.Response, but with any proto
type Response struct {
	Status     string
	StatusCode int
	Proto      string
	Header     textproto.MIMEHeader
	Body       []byte
	Request    *Request
}

func (r *Response) String() string {
	var s strings.Builder
	s.WriteString(r.Proto + " " + r.Status + EndLine)
	writeHeaders(&s, r.Header)
	s.WriteString(EndLine)
	s.Write(r.Body)
	return s.String()
}

func (r *Response) Write(w io.Writer) (err error) {
	_, err = w.Write([]byte(r.String()))
	return
}

func ReadResponse(r *bufio.Reader) (*Response, error) {
	tp := textproto.NewReader(r)

	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("empty response on RTSP request")
	}

	ss := strings.SplitN(line, " ", 3)
	if len(ss) != 3 {
		return nil, fmt.Errorf("malformed response: %s", line)
	}

	res := &Response{
		Status: ss[1] + " " + ss[2],
		Proto:  ss[0],
	}

	res.StatusCode, err = strconv.Atoi(ss[1])
	if err != nil {
		return nil, err
	}

	res.Header, err = tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	if res.Body, err = readBody(r, res.Header); err != nil {
		return nil, err
	}

	return res, nil
}

// headerOrder is the RTSP header order used by LIVE555 (VLC). Some servers
// (ex. Google Nest, Wowza) are sensitive to the order and ignore a request
// unless CSeq is the first header.
// https://github.com/AlexxIT/go2rtc/issues/2622
var headerOrder = []string{
	"CSeq",
	"Authorization",
	"User-Agent",
	"Accept",
	"Content-Type",
	"Range",
	"Transport",
	"Require",
	"Session",
	"Server",
	"Public",
	"Www-Authenticate",
	"Content-Length",
}

// writeHeaders writes the first value of each header in headerOrder, the key
// in any case (Header.Set stores CSeq as Cseq). Headers outside the list
// follow sorted, none is dropped.
func writeHeaders(s *strings.Builder, header textproto.MIMEHeader) {
	keys := make([]string, 0, len(header))
	for k, v := range header {
		if len(v) != 0 {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		if i, j := headerRank(a), headerRank(b); i != j {
			return i - j
		}
		return strings.Compare(a, b)
	})
	for _, k := range keys {
		s.WriteString(k + ": " + header[k][0] + EndLine)
	}
}

func headerRank(key string) int {
	for i, name := range headerOrder {
		if strings.EqualFold(key, name) {
			return i
		}
	}
	return len(headerOrder)
}

// Request like http.Request, but with any proto
type Request struct {
	Method string
	URL    *url.URL
	Proto  string
	Header textproto.MIMEHeader
	Body   []byte
}

func (r *Request) String() string {
	var s strings.Builder
	s.WriteString(r.Method + " " + r.URL.String() + " " + r.Proto + EndLine)
	writeHeaders(&s, r.Header)
	s.WriteString(EndLine)
	s.Write(r.Body)
	return s.String()
}

func (r *Request) Write(w io.Writer) (err error) {
	_, err = w.Write([]byte(r.String()))
	return
}

func ReadRequest(r *bufio.Reader) (*Request, error) {
	tp := textproto.NewReader(r)

	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}

	ss := strings.SplitN(line, " ", 3)
	if len(ss) != 3 {
		return nil, fmt.Errorf("wrong request: %s", line)
	}

	req := &Request{
		Method: ss[0],
		Proto:  ss[2],
	}

	req.URL, err = url.Parse(ss[1])
	if err != nil {
		return nil, err
	}

	req.Header, err = tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	if req.Body, err = readBody(r, req.Header); err != nil {
		return nil, err
	}

	return req, nil
}
