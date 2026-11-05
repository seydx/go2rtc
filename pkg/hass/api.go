package hass

import (
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	pion "github.com/pion/webrtc/v4"
)

type Message struct {
	ID      int             `json:"id"`
	Type    string          `json:"type"`
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Event   json.RawMessage `json:"event"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type ClientConfig struct {
	Configuration struct {
		ICEServers json.RawMessage `json:"iceServers"`
	} `json:"configuration"`
	DataChannel string `json:"dataChannel"`
}

type WebRTCEvent struct {
	Type      string                `json:"type"`
	SessionID string                `json:"session_id"`
	Answer    string                `json:"answer"`
	Candidate pion.ICECandidateInit `json:"candidate"`
	Message   string                `json:"message"`
}

type API struct {
	ws *websocket.Conn
	mu sync.Mutex
	id int
}

func NewAPI(url, token string) (*API, error) {
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}

	api := &API{ws: ws}
	if err = api.Auth(token); err != nil {
		_ = ws.Close()
		return nil, err
	}

	return api, nil
}

func (a *API) Auth(token string) error {
	var res ResponseAuth

	if err := a.ws.ReadJSON(&res); err != nil {
		return err
	}
	if res.Type != "auth_required" {
		return errors.New("hass: wrong type: " + res.Type)
	}

	s := `{"type":"auth","access_token":"` + token + `"}`
	if err := a.ws.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
		return err
	}
	if err := a.ws.ReadJSON(&res); err != nil {
		return err
	}
	if res.Type != "auth_ok" {
		return errors.New("hass: wrong type: " + res.Type)
	}

	return nil
}

func (a *API) Close() error {
	return a.ws.Close()
}

func (a *API) Send(msg map[string]any) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.id++
	msg["id"] = a.id
	return a.id, a.ws.WriteJSON(msg)
}

func (a *API) Read() (*Message, error) {
	var msg Message
	if err := a.ws.ReadJSON(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (a *API) Call(msg map[string]any, result any) error {
	id, err := a.Send(msg)
	if err != nil {
		return err
	}

	for {
		res, err := a.Read()
		if err != nil {
			return err
		}
		if res.ID != id || res.Type != "result" {
			continue
		}
		if !res.Success {
			return res.err()
		}
		return json.Unmarshal(res.Result, result)
	}
}

func (a *API) ClientConfig(entityID string) (*ClientConfig, error) {
	var config ClientConfig
	msg := map[string]any{"type": "camera/webrtc/get_client_config", "entity_id": entityID}
	if err := a.Call(msg, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func (a *API) SendOffer(entityID, offer string) (int, error) {
	return a.Send(map[string]any{"type": "camera/webrtc/offer", "entity_id": entityID, "offer": offer})
}

func (a *API) SendCandidate(entityID, sessionID string, candidate pion.ICECandidateInit) error {
	_, err := a.Send(map[string]any{
		"type":       "camera/webrtc/candidate",
		"entity_id":  entityID,
		"session_id": sessionID,
		"candidate":  candidate,
	})
	return err
}

func (a *API) GetWebRTCEntities() (map[string]string, error) {
	s := `{"id":1,"type":"get_states"}`
	if err := a.ws.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
		return nil, err
	}

	var res ResponseStates
	if err := a.ws.ReadJSON(&res); err != nil {
		return nil, err
	}
	if res.Type != "result" || !res.Success {
		return nil, errors.New("hass: wrong response")
	}

	entities := map[string]string{}

	for _, entity := range res.Result {
		if entity.Attributes.FrontendStreamType == "web_rtc" {
			entities[entity.Attributes.FriendlyName] = entity.EntityId
		}
	}

	return entities, nil
}

type ResponseAuth struct {
	Type string `json:"type"`
}

type ResponseStates struct {
	//Id      int    `json:"id"`
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Result  []struct {
		EntityId string `json:"entity_id"`
		//State      string `json:"state"`
		Attributes struct {
			//ModelName          string `json:"model_name"`
			//Brand              string `json:"brand"`
			FrontendStreamType string `json:"frontend_stream_type"`
			FriendlyName       string `json:"friendly_name"`
			//SupportedFeatures  int    `json:"supported_features"`
		} `json:"attributes"`
		//LastChanged time.Time `json:"last_changed"`
		//LastUpdated time.Time `json:"last_updated"`
		//Context     struct {
		//	Id       string      `json:"id"`
		//	ParentId interface{} `json:"parent_id"`
		//	UserId   interface{} `json:"user_id"`
		//} `json:"context"`
	} `json:"result"`
}

func (m *Message) err() error {
	if m.Error != nil {
		return errors.New("hass: " + m.Error.Message)
	}
	return errors.New("hass: wrong response")
}

func SupervisorToken() string {
	return os.Getenv("SUPERVISOR_TOKEN")
}
