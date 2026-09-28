package mqtt

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"time"
)

// Message is a received MQTT message in normalized form.
type Message struct {
	Topic    string
	Payload  []byte
	QoS      byte
	Retained bool
}

// Client is the unified data-plane handle; one per command, short-lived.
// Implementations exist for MQTT 3.1.1 and MQTT 5.0; dial picks one by
// the instance protocol version.
type Client interface {
	// Close disconnects the session; it never fails fatally.
	Close()
	// Publish sends one message; for QoS 1/2 it returns after the broker
	// acknowledgment (PUBACK/PUBCOMP).
	Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error
	// Subscribe registers a topic filter; matching messages go to the
	// handler until Close.
	Subscribe(ctx context.Context, filter string, qos byte, handler func(Message)) error
}

// dial connects to the broker and dispatches to the protocol
// implementation selected by the instance.
func dial(ctx context.Context, inst Instance, conn Connection, password string, timeout time.Duration) (Client, error) {
	switch inst.protocolVersion() {
	case ProtocolV5:
		return dialV5(ctx, inst, conn, password, timeout)
	default:
		return dialV3(ctx, inst, conn, password, timeout)
	}
}

// v3Schemes translates the canonical schemes to the ones the 3.1.1
// library accepts (mqtt->tcp, mqtts->ssl; ws/wss pass through).
var v3Schemes = map[string]string{
	"mqtt": "tcp", "mqtts": "ssl", "ws": "ws", "wss": "wss",
}

// brokerURL returns the instance URL with the scheme translated for the
// 3.1.1 client library. It is a dial parameter only and is never written
// back to the config.
func brokerURL(inst Instance) string {
	u, err := url.Parse(inst.URL)
	if err != nil {
		return inst.URL
	}
	if s, ok := v3Schemes[u.Scheme]; ok {
		u.Scheme = s
	}
	return u.String()
}

// clientID resolves the effective client ID: the connection's configured
// one, or a random muxcat-<pid>-<rand> (batch mode uses clean-start
// sessions, so a stable ID is never required).
func clientID(conn Connection) string {
	if conn.ClientID != "" {
		return conn.ClientID
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("muxcat-%d-%x", os.Getpid(), b)
}
