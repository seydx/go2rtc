package tuya

import (
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
