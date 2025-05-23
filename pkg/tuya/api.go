package tuya

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	"github.com/google/uuid"
	pionWebrtc "github.com/pion/webrtc/v4"
)

type TuyaClient struct {
	httpClient   *http.Client
	mqtt         *TuyaMQTT
	apiURL       string
	rtspURL      string
	hlsURL       string
	sessionId    string
	clientId     string
	clientSecret string
	deviceId     string
	accessToken  string
	refreshToken string
	expireTime   int64
	uid          string
	motoId       string
	auth         string
	skill        *Skill
	iceServers   []pionWebrtc.ICEServer
}

type Token struct {
	UID          string `json:"uid"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpireTime   int64  `json:"expire_time"`
}

type AudioAttributes struct {
	CallMode           []int `json:"call_mode"`           // 1 = one way, 2 = two way
	HardwareCapability []int `json:"hardware_capability"` // 1 = mic, 2 = speaker
}

type OpenApiICE struct {
	Urls       string `json:"urls"`
	Username   string `json:"username"`
	Credential string `json:"credential"`
	TTL        int    `json:"ttl"`
}

type WebICE struct {
	Urls       string `json:"urls"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

type P2PConfig struct {
	Ices []OpenApiICE `json:"ices"`
}

type AudioSkill struct {
	Channels   int `json:"channels"`
	DataBit    int `json:"dataBit"`
	CodecType  int `json:"codecType"`
	SampleRate int `json:"sampleRate"`
}

type VideoSkill struct {
	StreamType int    `json:"streamType"` // 2 = main stream (hd), 4 = sub stream (sd)
	ProfileId  string `json:"profileId,omitempty"`
	CodecType  int    `json:"codecType"` // 2 = H264, 4 = H265
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SampleRate int    `json:"sampleRate"`
}

type Skill struct {
	WebRTC int          `json:"webrtc"`
	Audios []AudioSkill `json:"audios"`
	Videos []VideoSkill `json:"videos"`
}

type WebRTConfig struct {
	AudioAttributes      AudioAttributes `json:"audio_attributes"`
	Auth                 string          `json:"auth"`
	ID                   string          `json:"id"`
	MotoID               string          `json:"moto_id"`
	P2PConfig            P2PConfig       `json:"p2p_config"`
	ProtocolVersion      string          `json:"protocol_version"`
	Skill                string          `json:"skill"`
	SupportsWebRTCRecord bool            `json:"supports_webrtc_record"`
	SupportsWebRTC       bool            `json:"supports_webrtc"`
	VedioClaritiy        int             `json:"vedio_clarity"`
	VideoClaritiy        int             `json:"video_clarity"`
	VideoClarities       []int           `json:"video_clarities"`
}

type OpenIoTHubConfig struct {
	Url       string `json:"url"`
	ClientID  string `json:"client_id"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	SinkTopic struct {
		IPC string `json:"ipc"`
	} `json:"sink_topic"`
	SourceSink struct {
		IPC string `json:"ipc"`
	} `json:"source_topic"`
	ExpireTime int `json:"expire_time"`
}

type WebRTCConfigResponse struct {
	Success bool        `json:"success"`
	Result  WebRTConfig `json:"result"`
	Msg     string      `json:"msg,omitempty"`
	Code    int         `json:"code,omitempty"`
}

type TokenResponse struct {
	Success bool   `json:"success"`
	Result  Token  `json:"result"`
	Msg     string `json:"msg,omitempty"`
	Code    int    `json:"code,omitempty"`
}

type AllocateRequest struct {
	Type string `json:"type"`
}

type AllocateResponse struct {
	Success bool `json:"success"`
	Result  struct {
		URL string `json:"url"`
	} `json:"result"`
	Msg  string `json:"msg,omitempty"`
	Code int    `json:"code,omitempty"`
}

type OpenIoTHubConfigRequest struct {
	UID      string `json:"uid"`
	UniqueID string `json:"unique_id"`
	LinkType string `json:"link_type"`
	Topics   string `json:"topics"`
}

type OpenIoTHubConfigResponse struct {
	Success bool             `json:"success"`
	Result  OpenIoTHubConfig `json:"result"`
	Msg     string           `json:"msg,omitempty"`
	Code    int              `json:"code,omitempty"`
}

func NewTuyaClient(openAPIURL string, deviceId string, uid string, clientId string, clientSecret string, streamMode string) (*TuyaClient, error) {
	client := &TuyaClient{
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		mqtt:         &TuyaMQTT{waiter: core.Waiter{}},
		apiURL:       openAPIURL,
		sessionId:    core.RandString(6, 62),
		clientId:     clientId,
		deviceId:     deviceId,
		clientSecret: clientSecret,
		uid:          uid,
	}

	if err := client.InitToken(); err != nil {
		return nil, fmt.Errorf("failed to initialize token: %w", err)
	}

	if streamMode == "rtsp" {
		if err := client.GetStreamUrl("rtsp"); err != nil {
			return nil, fmt.Errorf("failed to get RTSP URL: %w", err)
		}
	} else if streamMode == "hls" {
		if err := client.GetStreamUrl("hls"); err != nil {
			return nil, fmt.Errorf("failed to get HLS URL: %w", err)
		}
	} else {
		if err := client.InitDevice(); err != nil {
			return nil, fmt.Errorf("failed to initialize device: %w", err)
		}

		if err := client.StartMQTT(); err != nil {
			return nil, fmt.Errorf("failed to start MQTT: %w", err)
		}
	}

	return client, nil
}

func (c *TuyaClient) Close() {
	c.StopMQTT()
	c.httpClient.CloseIdleConnections()
}

func (c *TuyaClient) InitToken() (err error) {
	url := fmt.Sprintf("https://%s/v1.0/token?grant_type=1", c.apiURL)

	c.accessToken = ""
	c.refreshToken = ""

	body, err := c.Request("GET", url, nil)
	if err != nil {
		return err
	}

	var tokenResponse TokenResponse
	err = json.Unmarshal(body, &tokenResponse)
	if err != nil {
		return err
	}

	if !tokenResponse.Success {
		return fmt.Errorf(tokenResponse.Msg)
	}

	c.accessToken = tokenResponse.Result.AccessToken
	c.refreshToken = tokenResponse.Result.RefreshToken
	c.expireTime = tokenResponse.Result.ExpireTime

	return nil
}

func (c *TuyaClient) InitDevice() (err error) {
	url := fmt.Sprintf("https://%s/v1.0/users/%s/devices/%s/webrtc-configs", c.apiURL, c.uid, c.deviceId)

	body, err := c.Request("GET", url, nil)
	if err != nil {
		return err
	}

	var webRTCConfigResponse WebRTCConfigResponse
	err = json.Unmarshal(body, &webRTCConfigResponse)
	if err != nil {
		return err
	}

	if !webRTCConfigResponse.Success {
		return fmt.Errorf(webRTCConfigResponse.Msg)
	}

	err = json.Unmarshal([]byte(webRTCConfigResponse.Result.Skill), &c.skill)
	if err != nil {
		return err
	}

	iceServers, err := json.Marshal(&webRTCConfigResponse.Result.P2PConfig.Ices)
	if err != nil {
		return err
	}

	c.iceServers, err = webrtc.UnmarshalICEServers(iceServers)
	if err != nil {
		return err
	}

	c.motoId = webRTCConfigResponse.Result.MotoID
	c.auth = webRTCConfigResponse.Result.Auth

	return nil
}

func (c *TuyaClient) GetStreamUrl(streamType string) (err error) {
	url := fmt.Sprintf("https://%s/v1.0/devices/%s/stream/actions/allocate", c.apiURL, c.deviceId)

	request := &AllocateRequest{
		Type: streamType,
	}

	body, err := c.Request("POST", url, request)
	if err != nil {
		return err
	}

	var allosResponse AllocateResponse
	err = json.Unmarshal(body, &allosResponse)
	if err != nil {
		return err
	}

	if !allosResponse.Success {
		return fmt.Errorf(allosResponse.Msg)
	}

	switch streamType {
	case "rtsp":
		c.rtspURL = allosResponse.Result.URL
	case "hls":
		c.hlsURL = allosResponse.Result.URL
	default:
		return fmt.Errorf("unsupported stream type: %s", streamType)
	}

	return nil
}

func (c *TuyaClient) LoadHubConfig() (config *OpenIoTHubConfig, err error) {
	url := fmt.Sprintf("https://%s/v2.0/open-iot-hub/access/config", c.apiURL)

	request := &OpenIoTHubConfigRequest{
		UID:      c.uid,
		UniqueID: uuid.New().String(),
		LinkType: "mqtt",
		Topics:   "ipc",
	}

	body, err := c.Request("POST", url, request)
	if err != nil {
		return nil, err
	}

	var openIoTHubConfigResponse OpenIoTHubConfigResponse
	err = json.Unmarshal(body, &openIoTHubConfigResponse)
	if err != nil {
		return nil, err
	}

	if !openIoTHubConfigResponse.Success {
		return nil, fmt.Errorf(openIoTHubConfigResponse.Msg)
	}

	return &openIoTHubConfigResponse.Result, nil
}

func (c *TuyaClient) Request(method string, url string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	ts := time.Now().UnixNano() / 1000000
	sign := c.calBusinessSign(ts)

	req.Header.Set("Accept", "*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Control-Allow-Origin", "*")
	req.Header.Set("Access-Control-Allow-Methods", "*")
	req.Header.Set("Access-Control-Allow-Headers", "*")
	req.Header.Set("mode", "no-cors")
	req.Header.Set("client_id", c.clientId)
	req.Header.Set("access_token", c.accessToken)
	req.Header.Set("sign", sign)
	req.Header.Set("t", strconv.FormatInt(ts, 10))

	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	res, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, err
	}

	return res, nil
}

func (c *TuyaClient) getVideoCodecs() []*core.Codec {
	if len(c.skill.Videos) > 0 {
		codecs := make([]*core.Codec, 0)

		for _, video := range c.skill.Videos {
			name := core.CodecH264
			if c.isHEVC(video.StreamType) {
				name = core.CodecH265
			}

			codec := &core.Codec{
				Name:      name,
				ClockRate: uint32(video.SampleRate),
			}

			codecs = append(codecs, codec)
		}

		if len(codecs) > 0 {
			return codecs
		}
	}

	return nil
}

func (c *TuyaClient) getAudioCodecs() []*core.Codec {
	if len(c.skill.Audios) > 0 {
		codecs := make([]*core.Codec, 0)

		for _, audio := range c.skill.Audios {
			name := getAudioCodecName(&audio)

			codec := &core.Codec{
				Name:      name,
				ClockRate: uint32(audio.SampleRate),
				Channels:  uint8(audio.Channels),
			}
			codecs = append(codecs, codec)
		}

		if len(codecs) > 0 {
			return codecs
		}
	}

	return nil
}

// https://protect-us.ismartlife.me/
func getAudioCodecName(audioSkill *AudioSkill) string {
	switch audioSkill.CodecType {
	// case 100:
	// 	return "ADPCM"
	case 101:
		return core.CodecPCML
	case 102, 103, 104:
		return core.CodecAAC
	case 105:
		return core.CodecPCMU
	case 106:
		return core.CodecPCMA
	// case 107:
	// 	return "G726-32"
	// case 108:
	// 	return "SPEEX"
	case 109:
		return core.CodecMP3
	default:
		return core.CodecPCML
	}
}

func (c *TuyaClient) getStreamType(streamResolution string) int {
	// Default streamType if nothing is found
	defaultStreamType := 1

	if c.skill == nil || len(c.skill.Videos) == 0 {
		return defaultStreamType
	}

	// Find the highest and lowest resolution
	var highestResType = defaultStreamType
	var highestRes = 0
	var lowestResType = defaultStreamType
	var lowestRes = 0

	for _, video := range c.skill.Videos {
		res := video.Width * video.Height

		// Highest Resolution
		if res > highestRes {
			highestRes = res
			highestResType = video.StreamType
		}

		// Lower Resolution (or first if not set yet)
		if lowestRes == 0 || res < lowestRes {
			lowestRes = res
			lowestResType = video.StreamType
		}
	}

	// Return the streamType based on the selection
	switch streamResolution {
	case "hd":
		return highestResType
	case "sd":
		return lowestResType
	default:
		return defaultStreamType
	}
}

func (c *TuyaClient) isHEVC(streamType int) bool {
	for _, video := range c.skill.Videos {
		if video.StreamType == streamType {
			return video.CodecType == 4
		}
	}

	return false
}

func (c *TuyaClient) calBusinessSign(ts int64) string {
	data := fmt.Sprintf("%s%s%s%d", c.clientId, c.accessToken, c.clientSecret, ts)
	val := md5.Sum([]byte(data))
	res := fmt.Sprintf("%X", val)
	return res
}
