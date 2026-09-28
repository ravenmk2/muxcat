package mqtt

import (
	"context"
	"time"

	mqttv3 "github.com/eclipse/paho.mqtt.golang"

	"github.com/ravenmk2/muxcat/internal/output"
)

// clientV3 is the MQTT 3.1.1 client (paho.mqtt.golang).
type clientV3 struct {
	c        mqttv3.Client
	password string
	timeout  time.Duration
}

func dialV3(_ context.Context, inst Instance, conn Connection, password string, timeout time.Duration) (Client, error) {
	opts := mqttv3.NewClientOptions().
		AddBroker(brokerURL(inst)).
		SetClientID(clientID(conn)).
		SetUsername(conn.Username).
		SetPassword(password).
		SetCleanSession(true).
		SetAutoReconnect(false).
		SetProtocolVersion(4). // MQTT 3.1.1 explicitly; no 3.1 fallback
		SetConnectTimeout(timeout)
	if tc := tlsConfig(inst, conn); tc != nil {
		opts.SetTLSConfig(tc)
	}
	c := mqttv3.NewClient(opts)
	token := c.Connect()
	if err := waitToken(token, timeout); err != nil {
		return nil, classifyDialV3(err, password)
	}
	return &clientV3{c: c, password: password, timeout: timeout}, nil
}

// waitToken waits for a paho token up to the command timeout and returns
// its error; an elapsed deadline maps to TIMEOUT.
func waitToken(token mqttv3.Token, timeout time.Duration) error {
	if !token.WaitTimeout(timeout) {
		return output.NewError(output.CodeTimeout,
			"operation timed out", "increase --timeout or check the server")
	}
	return token.Error()
}

func (c *clientV3) Close() {
	c.c.Disconnect(250)
}

func (c *clientV3) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	token := c.c.Publish(topic, qos, retain, payload)
	if err := waitToken(token, c.timeout); err != nil {
		return classifyV3(err, c.password)
	}
	return nil
}

func (c *clientV3) Subscribe(_ context.Context, filter string, qos byte, handler func(Message)) error {
	token := c.c.Subscribe(filter, qos, func(_ mqttv3.Client, m mqttv3.Message) {
		handler(Message{Topic: m.Topic(), Payload: m.Payload(), QoS: m.Qos(), Retained: m.Retained()})
	})
	if err := waitToken(token, c.timeout); err != nil {
		return classifyV3(err, c.password)
	}
	return nil
}
