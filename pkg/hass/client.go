package hass

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/webrtc"
	pion "github.com/pion/webrtc/v4"
)

const connectTimeout = 15 * time.Second

type Client struct {
	conn     *webrtc.Conn
	api      *API
	entityID string

	mu        sync.Mutex
	sessionID string
	pending   []pion.ICECandidateInit
}

func NewClient(rawURL string) (*Client, error) {
	rawURL, rawFlags, _ := strings.Cut(rawURL, "#")
	flags, _ := url.ParseQuery(strings.ReplaceAll(rawFlags, "#", "&"))

	u, err := url.Parse(rawURL)
	if err != nil {
		// the url error quotes the token
		return nil, errors.New("hass: wrong url")
	}

	query := u.Query()

	entityID := query.Get("entity_id")
	if entityID == "" {
		return nil, errors.New("hass: no entity_id")
	}

	var uri, token string

	if u.Host == "supervisor" {
		uri = "ws://supervisor/core/websocket"
		token = SupervisorToken()
	} else {
		scheme := "ws"
		if query.Get("tls") == "1" {
			scheme = "wss"
		}
		uri = scheme + "://" + u.Host + "/api/websocket"
		token = query.Get("token")
	}

	if token == "" {
		return nil, errors.New("hass: no token")
	}

	hassAPI, err := NewAPI(uri, token)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = hassAPI.Close()
		}
	}()

	config, err := hassAPI.ClientConfig(entityID)
	if err != nil {
		return nil, err
	}

	conf := pion.Configuration{}
	if len(config.Configuration.ICEServers) > 0 {
		conf.ICEServers, _ = webrtc.UnmarshalICEServers(config.Configuration.ICEServers)
	}

	rtcAPI, err := webrtc.NewAPI()
	if err != nil {
		return nil, err
	}

	pc, err := rtcAPI.NewPeerConnection(conf)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = pc.Close()
		}
	}()

	query.Del("token")
	u.RawQuery = query.Encode()

	client := &Client{api: hassAPI, entityID: entityID}

	var connState core.Waiter

	conn := webrtc.NewConn(pc)
	conn.FormatName = "hass/webrtc"
	conn.Mode = core.ModeActiveProducer
	conn.Protocol = "ws"
	conn.URL = u.String()
	conn.Listen(func(msg any) {
		switch msg := msg.(type) {
		case *pion.ICECandidate:
			client.sendCandidate(msg.ToJSON())

		case pion.PeerConnectionState:
			switch msg {
			case pion.PeerConnectionStateConnecting:
			case pion.PeerConnectionStateConnected:
				connState.Done(nil)
			default:
				// Home Assistant ends the session together with the websocket
				_ = hassAPI.Close()
				connState.Done(errors.New("hass: webrtc " + msg.String()))
			}
		}
	})
	client.conn = conn

	audio := core.DirectionRecvonly
	if flags.Get("backchannel") == "1" {
		audio = core.DirectionSendRecv
	}

	// Nest wants audio, video, then the data channel it names
	medias := []*core.Media{
		{Kind: core.KindAudio, Direction: audio},
		{Kind: core.KindVideo, Direction: core.DirectionRecvonly},
	}
	if config.DataChannel != "" {
		medias = append(medias, &core.Media{Kind: config.DataChannel})
	}

	offer, err := conn.CreateOffer(medias)
	if err != nil {
		return nil, err
	}

	offerID, err := hassAPI.SendOffer(entityID, offer)
	if err != nil {
		return nil, err
	}

	go func() {
		connState.Done(client.readEvents(offerID))
		_ = conn.Interrupt()
	}()

	conn.CloseUnlessConnected(connectTimeout)

	if err = connState.Wait(); err != nil {
		return nil, err
	}

	return client, nil
}

func (c *Client) GetMedias() []*core.Media {
	return c.conn.GetMedias()
}

func (c *Client) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	return c.conn.GetTrack(media, codec)
}

func (c *Client) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	return c.conn.AddTrack(media, codec, track)
}

func (c *Client) Start() error {
	return c.conn.Start()
}

func (c *Client) Stop() error {
	_ = c.api.Close()
	return c.conn.Stop()
}

func (c *Client) Interrupt() error {
	_ = c.api.Close()
	return c.conn.Interrupt()
}

func (c *Client) IsClosed() bool {
	return c.conn.IsClosed()
}

func (c *Client) MarshalJSON() ([]byte, error) {
	return c.conn.MarshalJSON()
}

func (c *Client) readEvents(offerID int) error {
	for {
		msg, err := c.api.Read()
		if err != nil {
			return err
		}
		if msg.ID != offerID {
			continue
		}

		switch msg.Type {
		case "result":
			if !msg.Success {
				return msg.err()
			}

		case "event":
			var event WebRTCEvent
			if err = json.Unmarshal(msg.Event, &event); err != nil {
				return err
			}

			switch event.Type {
			case "session":
				c.setSession(event.SessionID)
			case "answer":
				if err = c.conn.SetAnswer(event.Answer); err != nil {
					return err
				}
			case "candidate":
				if event.Candidate.Candidate != "" {
					_ = c.conn.AddCandidate(event.Candidate.Candidate)
				}
			case "error":
				return errors.New("hass: " + event.Message)
			}
		}
	}
}

func (c *Client) sendCandidate(candidate pion.ICECandidateInit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sessionID == "" {
		c.pending = append(c.pending, candidate)
		return
	}
	_ = c.api.SendCandidate(c.entityID, c.sessionID, candidate)
}

func (c *Client) setSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sessionID = sessionID
	for _, candidate := range c.pending {
		_ = c.api.SendCandidate(c.entityID, sessionID, candidate)
	}
	c.pending = nil
}
