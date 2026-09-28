package amqp

import (
	"context"
	"strings"
	"time"

	"github.com/ravenmk2/muxcat/internal/output"
)

// QueueSpec describes a queue to declare.
type QueueSpec struct {
	Name       string
	Type       string // "" (broker default) | classic | quorum | stream
	Durable    bool
	AutoDelete bool
	Exclusive  bool
	Args       map[string]any
}

// ExchangeSpec describes an exchange to declare.
type ExchangeSpec struct {
	Name       string
	Type       string // direct | fanout | topic | headers
	Durable    bool
	AutoDelete bool
	Args       map[string]any
}

// Outgoing is a message to publish. Props holds the validated whitelist
// subset (see validPropKeys); each protocol maps it to its wire format.
type Outgoing struct {
	Body       []byte
	Persistent bool
	Props      map[string]any
	Headers    map[string]string
}

// QueueInfo is the normalized queue probe result. Type is "" when the
// protocol does not report it (0.9.1 passive declare).
type QueueInfo struct {
	Name      string
	Messages  int64
	Consumers int64
	Type      string
}

// ExchangeInfo is the normalized exchange probe result. Type is "" when
// the protocol does not report it.
type ExchangeInfo struct {
	Name string
	Type string
}

// PublishOutcome is the normalized settlement of one publish.
type PublishOutcome struct {
	Confirmed    bool   // broker confirmed/accepted the message
	Unroutable   bool   // mandatory-returned (0.9.1) / released (1.0)
	RejectReason string // 1.0 rejected: queue + reason supplied by the broker
}

// Delivery is a received message in normalized form.
type Delivery struct {
	Exchange    string
	RoutingKey  string
	Redelivered bool
	Payload     []byte
	Properties  map[string]any
}

// DeliveryContext is a received message plus its settlement handle.
type DeliveryContext interface {
	Delivery() *Delivery
	// Ack removes the message from the queue (destructive).
	Ack(ctx context.Context) error
	// Requeue returns the message to the queue (non-destructive).
	Requeue(ctx context.Context) error
	// Reject discards or dead-letters the message (destructive).
	Reject(ctx context.Context) error
}

// Receiver is an open consumer over a queue.
type Receiver interface {
	Receive(ctx context.Context) (DeliveryContext, error)
	Close() error
}

// Session is the unified data-plane handle; one per command, short-lived.
// Implementations exist for AMQP 0.9.1 and AMQP 1.0; openSession picks one
// by the instance protocol.
type Session interface {
	// Close releases the session; it never fails fatally.
	Close()
	DeclareQueue(ctx context.Context, spec QueueSpec) error
	DeleteQueue(ctx context.Context, name string, ifEmpty, ifUnused bool) error
	PurgeQueue(ctx context.Context, name string) (int, error)
	QueueInfo(ctx context.Context, name string) (*QueueInfo, error)
	DeclareExchange(ctx context.Context, spec ExchangeSpec) error
	DeleteExchange(ctx context.Context, name string, ifUnused bool) error
	ExchangeInfo(ctx context.Context, name string) (*ExchangeInfo, error)
	// Bind returns the binding path when the protocol has one (1.0), ""
	// otherwise (0.9.1).
	Bind(ctx context.Context, exchange, queue, routingKey string, args map[string]any) (string, error)
	Unbind(ctx context.Context, exchange, queue, routingKey string) error
	Publish(ctx context.Context, exchange, routingKey string, msg Outgoing) (PublishOutcome, error)
	// Get pulls a single message and settles it with mode
	// (peek|ack|reject) while its consumer/link is still open;
	// (nil, nil) means the queue is empty.
	Get(ctx context.Context, queue, mode string) (DeliveryContext, error)
	NewReceiver(ctx context.Context, queue string) (Receiver, error)
	// ServerInfo reports the broker product and version learned at
	// handshake; either may be "".
	ServerInfo() (product, version string)
}

// openSession dials the broker and dispatches to the protocol
// implementation selected by the instance.
func openSession(ctx context.Context, inst Instance, conn Connection, vhost, password string, timeout time.Duration) (Session, error) {
	switch inst.protocol() {
	case Protocol091:
		return dial091(ctx, inst, conn, vhost, password, timeout)
	default:
		return dial10(ctx, inst, conn, vhost, password, timeout)
	}
}

// --- --props whitelist (portable subset) ---

// validPropKeys lists the --props keys accepted by exchange publish, in
// documentation order. Unknown keys are rejected with CONFIG_INVALID.
var validPropKeys = []string{
	"message_id", "correlation_id", "content_type", "content_encoding",
	"reply_to", "type", "expiration", "priority", "timestamp",
	"user_id", "app_id",
}

var validPropSet = func() map[string]bool {
	m := make(map[string]bool, len(validPropKeys))
	for _, k := range validPropKeys {
		m[k] = true
	}
	return m
}()

// stringPropKeys are the props keys that must be JSON strings.
var stringPropKeys = map[string]bool{
	"message_id": true, "correlation_id": true, "content_type": true,
	"content_encoding": true, "reply_to": true, "type": true,
	"expiration": true, "user_id": true, "app_id": true,
}

// validateProps checks the --props whitelist and value types.
func validateProps(props map[string]any) error {
	for k, v := range props {
		if !validPropSet[k] {
			return output.NewError(output.CodeConfigInvalid,
				"unknown --props key: "+k,
				"valid keys: "+strings.Join(validPropKeys, ", "))
		}
		switch k {
		case "priority":
			f, ok := v.(float64)
			if !ok || f != float64(int64(f)) || f < 0 || f > 255 {
				return output.NewError(output.CodeConfigInvalid,
					"--props priority must be an integer between 0 and 255", "")
			}
		case "timestamp":
			switch v := v.(type) {
			case float64: // unix seconds
			case string: // RFC 3339
				if _, err := time.Parse(time.RFC3339, v); err != nil {
					return output.NewError(output.CodeConfigInvalid,
						"--props timestamp must be unix seconds or an RFC 3339 string: "+v, "")
				}
			default:
				return output.NewError(output.CodeConfigInvalid,
					"--props timestamp must be unix seconds or an RFC 3339 string", "")
			}
		default:
			if stringPropKeys[k] {
				if _, ok := v.(string); !ok {
					return output.NewError(output.CodeConfigInvalid,
						"--props "+k+" must be a string", "")
				}
			}
		}
	}
	return nil
}

// propString reads a string prop; "" when absent.
func propString(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

// propTime resolves the timestamp prop to a time.Time.
func propTime(props map[string]any) (time.Time, bool) {
	switch v := props["timestamp"].(type) {
	case float64:
		return time.Unix(int64(v), 0), true
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseHeaders parses the --headers k=v,... flag.
func parseHeaders(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, output.NewError(output.CodeConfigInvalid,
				"invalid --headers entry: "+pair, "expected comma-separated k=v pairs, e.g. tenant=eu,trace=1")
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}
