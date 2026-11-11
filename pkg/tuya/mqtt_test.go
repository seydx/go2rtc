package tuya

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/stretchr/testify/require"
)

// token is the broker's acknowledgement: done when it arrived, with err
type token struct {
	done chan struct{}
	err  error
}

func (t *token) Wait() bool {
	<-t.done
	return true
}

func (t *token) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (t *token) Done() <-chan struct{} { return t.done }
func (t *token) Error() error          { return t.err }

// broker stands in for paho, with the acknowledgements it gives
type broker struct {
	mqtt.Client
	pub, sub *token
}

func (b *broker) Publish(string, byte, bool, any) mqtt.Token { return b.pub }
func (b *broker) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	return b.sub
}
func (b *broker) Disconnect(uint) {}

// never is held back like paho does while it waits for a lost connection to
// come back; the wait ends with the test
func never(t *testing.T) *token {
	ack := &token{done: make(chan struct{})}
	t.Cleanup(func() { close(ack.done) })
	return ack
}

func acked(err error) *token {
	ack := &token{done: make(chan struct{}), err: err}
	close(ack.done)
	return ack
}

func goneBroker(t *testing.T) *broker {
	return &broker{pub: never(t), sub: never(t)}
}

func setAckTimeout(t *testing.T, d time.Duration) {
	old := mqttAckTimeout
	t.Cleanup(func() { mqttAckTimeout = old })
	mqttAckTimeout = d
}

// returnsWithin runs f and fails the test if it doesn't return in time
func returnsWithin(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("still waiting for the broker")
		return nil
	}
}

// Stopping a stream says goodbye to the camera over MQTT. With the broker
// gone that goodbye is never acknowledged, and Stop has to return anyway.
func TestStopReturnsWithoutBroker(t *testing.T) {
	setAckTimeout(t, 100*time.Millisecond)
	c := &TuyaMqttClient{client: goneBroker(t)}

	returnsWithin(t, 3*time.Second, func() error {
		c.Stop()
		return nil
	})
}

func TestSignalingFailsWithoutBroker(t *testing.T) {
	setAckTimeout(t, 100*time.Millisecond)

	for name, send := range map[string]func(c *TuyaMqttClient) error{
		"disconnect": (*TuyaMqttClient).SendDisconnect,
		"wake up":    func(c *TuyaMqttClient) error { return c.WakeUp("key") },
		"wake up subscribe": func(c *TuyaMqttClient) error {
			c.client.(*broker).pub = acked(nil)
			return c.WakeUp("key")
		},
		"subscribe": func(c *TuyaMqttClient) error {
			c.onConnect(c.client)
			return c.waiter.Wait()
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := &TuyaMqttClient{client: goneBroker(t)}
			err := returnsWithin(t, 3*time.Second, func() error { return send(c) })
			require.ErrorContains(t, err, "no acknowledgement")
		})
	}
}

// An acknowledgement in time decides: nil, or the broker's error.
func TestSignalingWithBroker(t *testing.T) {
	c := &TuyaMqttClient{client: &broker{pub: acked(nil), sub: acked(nil)}}
	require.NoError(t, c.SendDisconnect())
	require.NoError(t, c.WakeUp("key"))
	c.onConnect(c.client)
	require.NoError(t, c.waiter.Wait())

	refused := errors.New("not authorized")
	c = &TuyaMqttClient{client: &broker{pub: acked(refused), sub: acked(refused)}}
	require.ErrorIs(t, c.SendDisconnect(), refused)
	require.ErrorIs(t, c.WakeUp("key"), refused)
	c.onConnect(c.client)
	require.ErrorIs(t, c.waiter.Wait(), refused)
}

// recorder is a broker that keeps what was published
type recorder struct {
	broker
	topics   []string
	payloads [][]byte
}

func (r *recorder) Publish(topic string, _ byte, _ bool, payload any) mqtt.Token {
	r.topics = append(r.topics, topic)
	r.payloads = append(r.payloads, payload.([]byte))
	return acked(nil)
}

// A camera behind a hub is addressed through the hub, with its node id.
func TestSubDeviceIsReachedThroughTheHub(t *testing.T) {
	rec := &recorder{}
	c := &TuyaMqttClient{client: rec, deviceId: "camera", gatewayId: "hub", nodeId: "node", publishTopic: "/av/moto/moto/u/hub"}
	require.NoError(t, c.SendDisconnect())

	var msg MqttMessage
	require.NoError(t, json.Unmarshal(rec.payloads[0], &msg))
	require.Equal(t, "hub", msg.Data.Header.To)
	require.Equal(t, "node", msg.Data.Header.SubDevID)

	c = &TuyaMqttClient{client: &recorder{}, deviceId: "camera"}
	require.Equal(t, "camera", c.target())
}

func TestRefusedSessionIsReported(t *testing.T) {
	for msg, want := range map[string]error{
		`{"mode":"webrtc","sdk_code":12}`:      errNoFreeStream,
		`{"mode":"webrtc","info":"refused"}`:   errNoFreeStream,
		`{"mode":"webrtc","close_reason":100}`: nil,
	} {
		var got error
		c := &TuyaMqttClient{handleDisconnect: func(err error) { got = err }}
		c.onMqttDisconnect(&MqttMessage{Data: MqttFrame{Message: json.RawMessage(msg)}})
		require.Error(t, got, msg)
		if want != nil {
			require.ErrorIs(t, got, want, msg)
		} else {
			require.NotErrorIs(t, got, errNoFreeStream, msg)
		}
	}
}

func TestWakeUpSignalIsPassedOn(t *testing.T) {
	var woken int
	c := &TuyaMqttClient{handleWakeUp: func() { woken++ }}
	c.onLowPowerMessage(nil, &message{payload: []byte(`{"protocol":4,"data":{"dps":{"149":true}}}`)})
	c.onLowPowerMessage(nil, &message{payload: []byte(`{"protocol":4,"data":{"dps":{"149":false}}}`)})
	require.Equal(t, 1, woken)
}

type message struct {
	mqtt.Message
	payload []byte
}

func (m *message) Payload() []byte { return m.payload }
