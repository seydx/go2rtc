package onvif

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const PathDevice = "/onvif/device_service"

type Client struct {
	url *url.URL

	deviceURL string
	mediaURL  string
	imaginURL string
}

func NewClient(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	baseURL := "http://" + u.Host

	client := &Client{url: u}

	// Set default media URL before trying to get capabilities
	client.mediaURL = baseURL + "/onvif/media_service"
	client.imaginURL = baseURL + "/onvif/imaging_service"

	if u.Path == "" {
		client.deviceURL = baseURL + PathDevice
	} else {
		client.deviceURL = baseURL + u.Path
	}

	b, err := client.DeviceRequest(DeviceGetCapabilities)
	if err != nil {
		baseURL = "https://" + u.Host

		// Set default media URL before trying to get capabilities
		client.mediaURL = baseURL + "/onvif/media_service"
		client.imaginURL = baseURL + "/onvif/imaging_service"

		if u.Path == "" {
			client.deviceURL = baseURL + PathDevice
		} else {
			client.deviceURL = baseURL + u.Path
		}

		b, err = client.DeviceRequest(DeviceGetCapabilities)
		if err != nil {
			return nil, err
		}
	}

	// Update URLs if found in capabilities
	if mediaAddr := FindTagValue(b, "Media.+?XAddr"); mediaAddr != "" {
		client.mediaURL = mediaAddr
	}
	if imagingAddr := FindTagValue(b, "Imaging.+?XAddr"); imagingAddr != "" {
		client.imaginURL = imagingAddr
	}

	return client, nil
}

func (c *Client) GetURI() (string, error) {
	query := c.url.Query()

	token := query.Get("subtype")

	// support empty
	if i := atoi(token); i >= 0 {
		tokens, err := c.GetProfilesTokens()
		if err != nil {
			return "", err
		}
		if i >= len(tokens) {
			return "", errors.New("onvif: wrong subtype")
		}
		token = tokens[i]
	}

	getUri := c.GetStreamUri
	if query.Has("snapshot") {
		getUri = c.GetSnapshotUri
	}

	b, err := getUri(token)
	if err != nil {
		return "", err
	}

	rawURL := FindTagValue(b, "Uri")
	rawURL = strings.TrimSpace(html.UnescapeString(rawURL))

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	if u.User == nil && c.url.User != nil {
		u.User = c.url.User
	}

	return u.String(), nil
}

func (c *Client) GetName() (string, error) {
	b, err := c.DeviceRequest(DeviceGetDeviceInformation)
	if err != nil {
		return "", err
	}

	return FindTagValue(b, "Manufacturer") + " " + FindTagValue(b, "Model"), nil
}

func (c *Client) GetProfilesTokens() ([]string, error) {
	b, err := c.MediaRequest(MediaGetProfiles)
	if err != nil {
		return nil, err
	}

	var tokens []string

	re := regexp.MustCompile(`Profiles.+?token="([^"]+)`)
	for _, s := range re.FindAllStringSubmatch(string(b), 10) {
		tokens = append(tokens, s[1])
	}

	return tokens, nil
}

func (c *Client) HasSnapshots() bool {
	b, err := c.GetServiceCapabilities()
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `SnapshotUri="true"`)
}

func (c *Client) GetProfile(token string) ([]byte, error) {
	return c.Request(
		c.mediaURL, `<trt:GetProfile><trt:ProfileToken>`+token+`</trt:ProfileToken></trt:GetProfile>`,
	)
}

func (c *Client) GetVideoSourceConfiguration(token string) ([]byte, error) {
	return c.Request(c.mediaURL, `<trt:GetVideoSourceConfiguration>
	<trt:ConfigurationToken>`+token+`</trt:ConfigurationToken>
</trt:GetVideoSourceConfiguration>`)
}

func (c *Client) GetStreamUri(token string) ([]byte, error) {
	return c.Request(c.mediaURL, `<trt:GetStreamUri>
	<trt:StreamSetup>
		<tt:Stream>RTP-Unicast</tt:Stream>
		<tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport>
	</trt:StreamSetup>
	<trt:ProfileToken>`+token+`</trt:ProfileToken>
</trt:GetStreamUri>`)
}

func (c *Client) GetSnapshotUri(token string) ([]byte, error) {
	return c.Request(
		c.imaginURL, `<trt:GetSnapshotUri><trt:ProfileToken>`+token+`</trt:ProfileToken></trt:GetSnapshotUri>`,
	)
}

func (c *Client) GetServiceCapabilities() ([]byte, error) {
	// some cameras answer GetServiceCapabilities for media only for path = "/onvif/media"
	return c.Request(
		c.mediaURL, `<trt:GetServiceCapabilities />`,
	)
}

func (c *Client) DeviceRequest(operation string) ([]byte, error) {
	switch operation {
	case DeviceGetServices:
		operation = `<tds:GetServices><tds:IncludeCapability>true</tds:IncludeCapability></tds:GetServices>`
	case DeviceGetCapabilities:
		operation = `<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`
	default:
		operation = `<tds:` + operation + `/>`
	}
	return c.Request(c.deviceURL, operation)
}

func (c *Client) MediaRequest(operation string) ([]byte, error) {
	operation = `<trt:` + operation + `/>`
	return c.Request(c.mediaURL, operation)
}

func (c *Client) Request(rawUrl, body string) ([]byte, error) {
	if rawUrl == "" {
		return nil, errors.New("onvif: unsupported service")
	}

	e := NewEnvelopeWithUser(c.url.User)
	e.Append(body)

	u, err := url.Parse(rawUrl)
	if err != nil {
		return nil, err
	}

	// Determine if HTTPS is needed
	isHTTPS := strings.ToLower(u.Scheme) == "https"

	// Ensure we have a port
	host := u.Host
	if !strings.Contains(host, ":") {
		if isHTTPS {
			host = host + ":443" // Standard HTTPS port
		} else {
			host = host + ":80" // Standard HTTP port
		}
	}

	var conn net.Conn

	if isHTTPS {
		// HTTPS connection with TLS
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true, // Accept self-signed certificates
		}

		// Connect with TLS
		tlsConn, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 5 * time.Second},
			"tcp",
			host,
			tlsConfig,
		)
		if err != nil {
			return nil, fmt.Errorf("TLS connection error: %w", err)
		}
		conn = tlsConn
	} else {
		// Plain HTTP connection
		tcpConn, err := net.DialTimeout("tcp", host, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("TCP connection error: %w", err)
		}
		conn = tcpConn
	}

	defer conn.Close()

	// Send request
	httpReq := fmt.Sprintf("POST %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Content-Type: application/soap+xml;charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n"+
		"\r\n%s", u.Path, u.Host, len(e.Bytes()), e.Bytes())

	if _, err = conn.Write([]byte(httpReq)); err != nil {
		return nil, err
	}

	// Read full response first
	var fullResponse []byte
	var xmlFound bool
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			fullResponse = append(fullResponse, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	// Look for XML in complete response
	if idx := bytes.Index(fullResponse, []byte("<?xml")); idx >= 0 {
		xmlFound = true
		fullResponse = fullResponse[idx:]
	}

	if xmlFound {
		if isAuthError(fullResponse) {
			return nil, errors.New("not authorized")
		}

		if isFault(fullResponse) {
			reason := FindTagValue(fullResponse, "Text")
			return nil, errors.New(reason)
		}

		return fullResponse, nil
	}

	// No XML found - might be an error response
	if idx := bytes.Index(fullResponse, []byte("\r\n\r\n")); idx >= 0 {
		// Return error message
		return nil, errors.New(string(fullResponse[idx+4:]))
	}

	return nil, errors.New("no XML found in response")
}

func isFault(b []byte) bool {
	return bytes.Contains(b, []byte("Fault"))
}

func isAuthError(xmlData []byte) bool {
	return isFault(xmlData) && bytes.Contains(xmlData, []byte("NotAuthorized"))
}
