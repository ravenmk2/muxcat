package amqp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	amqp10 "github.com/Azure/go-amqp"
	"github.com/rabbitmq/rabbitmq-amqp-go-client/pkg/rabbitmqamqp"

	"github.com/ravenmk2/muxcat/internal/output"
)

func init() {
	// The 1.0 client logs dial failures (including the dial URI, which
	// carries the username) through slog; muxcat renders errors itself.
	rabbitmqamqp.SetSlogHandler(slog.NewTextHandler(io.Discard, nil))
}

// session10 is the AMQP 1.0 session: one AmqpConnection with its
// RabbitMQ-specific management link. Publishers are cached per target so
// --count loops reuse one link.
type session10 struct {
	conn     *rabbitmqamqp.AmqpConnection
	mgmt     *rabbitmqamqp.AmqpManagement
	pubs     map[string]*rabbitmqamqp.Publisher
	password string
}

func dial10(ctx context.Context, inst Instance, conn Connection, vhost, password string, timeout time.Duration) (Session, error) {
	uri := dialURI(inst, conn, vhost, password)
	opts := &rabbitmqamqp.AmqpConnOptions{
		// A CLI command is short-lived; auto-recovery would only hide
		// failures and stall the exit.
		RecoveryConfiguration: &rabbitmqamqp.RecoveryConfiguration{ActiveRecovery: false},
		TLSConfig:             tlsConfig(inst, conn),
	}
	c, err := rabbitmqamqp.Dial(ctx, uri, opts)
	if err != nil {
		return nil, classifyDial(err, password, Protocol10, vhost)
	}
	return &session10{
		conn:     c,
		mgmt:     c.Management(),
		pubs:     map[string]*rabbitmqamqp.Publisher{},
		password: password,
	}, nil
}

func (s *session10) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, p := range s.pubs {
		_ = p.Close(ctx)
	}
	_ = s.conn.Close(ctx)
}

func (s *session10) ServerInfo() (product, version string) {
	product, _ = s.conn.Properties()["product"].(string)
	version, _ = s.conn.Properties()["version"].(string)
	return product, version
}

func (s *session10) DeclareQueue(ctx context.Context, spec QueueSpec) error {
	var qs rabbitmqamqp.IQueueSpecification
	switch spec.Type {
	case "quorum":
		qs = &rabbitmqamqp.QuorumQueueSpecification{Name: spec.Name, Arguments: spec.Args}
	case "stream":
		qs = &rabbitmqamqp.StreamQueueSpecification{Name: spec.Name, Arguments: spec.Args}
	case "classic":
		qs = &rabbitmqamqp.ClassicQueueSpecification{
			Name: spec.Name, IsAutoDelete: spec.AutoDelete, IsExclusive: spec.Exclusive,
			Arguments: spec.Args,
		}
	default:
		qs = &rabbitmqamqp.CustomQueueSpecification{
			Name: spec.Name, IsAutoDelete: spec.AutoDelete, IsExclusive: spec.Exclusive,
			Arguments: spec.Args,
		}
	}
	if _, err := s.mgmt.DeclareQueue(ctx, qs); err != nil {
		return classify10(err, s.password)
	}
	return nil
}

func (s *session10) DeleteQueue(ctx context.Context, name string, ifEmpty, ifUnused bool) error {
	// The 1.0 management DeleteQueue has no if-empty/if-unused options;
	// emulate them with a QueueInfo pre-check (a race is acceptable for a
	// CLI).
	if ifEmpty || ifUnused {
		info, err := s.QueueInfo(ctx, name)
		if err != nil {
			return err
		}
		if ifEmpty && info.Messages > 0 {
			return output.NewError(output.CodeQueryError,
				fmt.Sprintf("queue %s is not empty (%d messages)", name, info.Messages),
				"purge it first, or delete without --if-empty")
		}
		if ifUnused && info.Consumers > 0 {
			return output.NewError(output.CodeQueryError,
				fmt.Sprintf("queue %s is in use (%d consumers)", name, info.Consumers),
				"delete without --if-unused to force it")
		}
	}
	if err := s.mgmt.DeleteQueue(ctx, name); err != nil {
		return classify10(err, s.password)
	}
	return nil
}

func (s *session10) PurgeQueue(ctx context.Context, name string) (int, error) {
	n, err := s.mgmt.PurgeQueue(ctx, name)
	if err != nil {
		return 0, classify10(err, s.password)
	}
	return n, nil
}

func (s *session10) QueueInfo(ctx context.Context, name string) (*QueueInfo, error) {
	info, err := s.mgmt.QueueInfo(ctx, name)
	if err != nil {
		return nil, classify10(err, s.password)
	}
	return &QueueInfo{
		Name:      info.Name(),
		Messages:  int64(info.MessageCount()),
		Consumers: int64(info.ConsumerCount()),
		Type:      string(info.Type()),
	}, nil
}

func (s *session10) DeclareExchange(ctx context.Context, spec ExchangeSpec) error {
	var es rabbitmqamqp.IExchangeSpecification
	switch spec.Type {
	case "fanout":
		es = &rabbitmqamqp.FanOutExchangeSpecification{Name: spec.Name, IsAutoDelete: spec.AutoDelete, Arguments: spec.Args}
	case "topic":
		es = &rabbitmqamqp.TopicExchangeSpecification{Name: spec.Name, IsAutoDelete: spec.AutoDelete, Arguments: spec.Args}
	case "headers":
		es = &rabbitmqamqp.HeadersExchangeSpecification{Name: spec.Name, IsAutoDelete: spec.AutoDelete, Arguments: spec.Args}
	default:
		es = &rabbitmqamqp.DirectExchangeSpecification{Name: spec.Name, IsAutoDelete: spec.AutoDelete, Arguments: spec.Args}
	}
	if _, err := s.mgmt.DeclareExchange(ctx, es); err != nil {
		return classify10(err, s.password)
	}
	return nil
}

func (s *session10) DeleteExchange(ctx context.Context, name string, ifUnused bool) error {
	if ifUnused {
		return output.NewError(output.CodeUnsupportedOperation,
			"exchange delete --if-unused is not available over AMQP 1.0",
			"the 1.0 management interface cannot query exchange usage; check bindings with rmq binding ls, or use --protocol 0.9.1")
	}
	if err := s.mgmt.DeleteExchange(ctx, name); err != nil {
		return classify10(err, s.password)
	}
	return nil
}

func (s *session10) ExchangeInfo(_ context.Context, _ string) (*ExchangeInfo, error) {
	// The 1.0 management interface has no exchange query: a raw GET on
	// /exchanges/{name} crashes broker-side (function_clause on RabbitMQ
	// 4.3), and re-declaring for existence would create the exchange.
	return nil, output.NewError(output.CodeUnsupportedOperation,
		"exchange show is not available over AMQP 1.0",
		"the 1.0 management interface cannot query exchanges; use rmq exchange show (Management API), or --protocol 0.9.1")
}

func (s *session10) Bind(ctx context.Context, exchange, queue, routingKey string, args map[string]any) (string, error) {
	path, err := s.mgmt.Bind(ctx, &rabbitmqamqp.ExchangeToQueueBindingSpecification{
		SourceExchange:   exchange,
		DestinationQueue: queue,
		BindingKey:       routingKey,
		Arguments:        args,
	})
	if err != nil {
		return "", classify10(err, s.password)
	}
	return path, nil
}

func (s *session10) Unbind(ctx context.Context, exchange, queue, routingKey string) error {
	// Unbind needs the server-generated binding path; reconstruct it from
	// the (exchange, queue, key) triple, matching the client's own format
	// (empty args). Bindings declared with arguments cannot be addressed
	// this way.
	path := fmt.Sprintf("/bindings/src=%s;dstq=%s;key=%s;args=",
		encodePathSegment(exchange), encodePathSegment(queue), encodePathSegment(routingKey))
	if err := s.mgmt.Unbind(ctx, path); err != nil {
		return classify10(err, s.password)
	}
	return nil
}

// encodePathSegment percent-encodes one address path segment the way the
// 1.0 client does (QueryEscape with %20 for space).
func encodePathSegment(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// message10 maps the normalized Outgoing to an AMQP 1.0 message.
func message10(msg Outgoing) *amqp10.Message {
	m := rabbitmqamqp.NewMessage(msg.Body)
	props := msg.Props
	var header *amqp10.MessageHeader
	if msg.Persistent || props["expiration"] != nil || props["priority"] != nil {
		header = &amqp10.MessageHeader{Durable: msg.Persistent}
		if exp := propString(props, "expiration"); exp != "" {
			if ms, err := parseMillis(exp); err == nil {
				header.TTL = ms
			}
		}
		if f, ok := props["priority"].(float64); ok {
			header.Priority = uint8(f)
		}
	}
	m.Header = header
	mp := &amqp10.MessageProperties{}
	set := false
	if v := propString(props, "message_id"); v != "" {
		mp.MessageID = v
		set = true
	}
	if v := propString(props, "correlation_id"); v != "" {
		mp.CorrelationID = v
		set = true
	}
	if v := propString(props, "content_type"); v != "" {
		mp.ContentType = &v
		set = true
	}
	if v := propString(props, "content_encoding"); v != "" {
		mp.ContentEncoding = &v
		set = true
	}
	if v := propString(props, "reply_to"); v != "" {
		mp.ReplyTo = &v
		set = true
	}
	if v := propString(props, "type"); v != "" {
		mp.Subject = &v
		set = true
	}
	if v := propString(props, "user_id"); v != "" {
		mp.UserID = []byte(v)
		set = true
	}
	if t, ok := propTime(props); ok {
		mp.CreationTime = &t
		set = true
	}
	if set {
		m.Properties = mp
	}
	// app_id has no 1.0 counterpart; the command layer warns and drops it.
	if len(msg.Headers) > 0 {
		ap := make(map[string]any, len(msg.Headers))
		for k, v := range msg.Headers {
			ap[k] = v
		}
		m.ApplicationProperties = ap
	}
	return m
}

// parseMillis parses an expiration value in milliseconds.
func parseMillis(s string) (time.Duration, error) {
	var ms int64
	if _, err := fmt.Sscanf(s, "%d", &ms); err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (s *session10) publisher(ctx context.Context, exchange, routingKey string) (*rabbitmqamqp.Publisher, error) {
	key := exchange + "\x00" + routingKey
	if p, ok := s.pubs[key]; ok {
		return p, nil
	}
	p, err := s.conn.NewPublisher(ctx, &rabbitmqamqp.ExchangeAddress{Exchange: exchange, Key: routingKey}, nil)
	if err != nil {
		return nil, classify10(err, s.password)
	}
	s.pubs[key] = p
	return p, nil
}

func (s *session10) Publish(ctx context.Context, exchange, routingKey string, msg Outgoing) (PublishOutcome, error) {
	var oc PublishOutcome
	p, err := s.publisher(ctx, exchange, routingKey)
	if err != nil {
		return oc, err
	}
	res, err := p.Publish(ctx, message10(msg))
	if err != nil {
		return oc, classify10(err, s.password)
	}
	switch outcome := res.Outcome.(type) {
	case *amqp10.StateAccepted:
		oc.Confirmed = true
	case *amqp10.StateReleased:
		oc.Unroutable = true
	case *amqp10.StateRejected:
		oc.RejectReason = "rejected by the broker"
		if res.MessageRejectedError != nil {
			oc.RejectReason = res.MessageRejectedError.Error()
		}
	case *amqp10.StateModified:
		oc.Unroutable = true
	default:
		_ = outcome
	}
	return oc, nil
}

// delivery10 adapts a 1.0 delivery to DeliveryContext.
type delivery10 struct {
	dc rabbitmqamqp.IDeliveryContext
	n  *Delivery
}

func (d *delivery10) Delivery() *Delivery { return d.n }

func (d *delivery10) Ack(ctx context.Context) error     { return d.dc.Accept(ctx) }
func (d *delivery10) Requeue(ctx context.Context) error { return d.dc.Requeue(ctx) }
func (d *delivery10) Reject(ctx context.Context) error  { return d.dc.Discard(ctx, nil) }

// normalize10 converts a 1.0 message to the normalized Delivery. The
// broker annotates deliveries with x-exchange/x-routing-key.
func normalize10(msg *amqp10.Message) *Delivery {
	n := &Delivery{Payload: msg.GetData(), Properties: map[string]any{}}
	if v, ok := msg.Annotations["x-exchange"].(string); ok {
		n.Exchange = v
	}
	if v, ok := msg.Annotations["x-routing-key"].(string); ok {
		n.RoutingKey = v
	}
	if msg.Header != nil {
		n.Redelivered = msg.Header.DeliveryCount > 0
		if msg.Header.Durable {
			n.Properties["durable"] = true
		}
		if msg.Header.Priority > 0 {
			n.Properties["priority"] = msg.Header.Priority
		}
		if msg.Header.TTL > 0 {
			n.Properties["expiration"] = fmt.Sprintf("%d", msg.Header.TTL.Milliseconds())
		}
	}
	if p := msg.Properties; p != nil {
		if v, ok := p.MessageID.(string); ok && v != "" {
			n.Properties["message_id"] = v
		}
		if v, ok := p.CorrelationID.(string); ok && v != "" {
			n.Properties["correlation_id"] = v
		}
		if p.ContentType != nil && *p.ContentType != "" {
			n.Properties["content_type"] = *p.ContentType
		}
		if p.ContentEncoding != nil && *p.ContentEncoding != "" {
			n.Properties["content_encoding"] = *p.ContentEncoding
		}
		if p.ReplyTo != nil && *p.ReplyTo != "" {
			n.Properties["reply_to"] = *p.ReplyTo
		}
		if p.Subject != nil && *p.Subject != "" {
			n.Properties["type"] = *p.Subject
		}
		if len(p.UserID) > 0 {
			n.Properties["user_id"] = string(p.UserID)
		}
		if p.CreationTime != nil {
			n.Properties["timestamp"] = p.CreationTime.Format(time.RFC3339)
		}
	}
	if len(msg.ApplicationProperties) > 0 {
		n.Properties["headers"] = msg.ApplicationProperties
	}
	return n
}

func (s *session10) newConsumer(ctx context.Context, queue string) (*rabbitmqamqp.Consumer, error) {
	c, err := s.conn.NewConsumer(ctx, queue, nil)
	if err != nil {
		return nil, classify10(err, s.password)
	}
	return c, nil
}

// getWait caps how long a 1.0 queue get blocks waiting for a message: a
// get is a snapshot operation (0.9.1 basic.get returns immediately), so an
// empty queue must not stall for the whole command timeout.
const getWait = 5 * time.Second

func (s *session10) Get(ctx context.Context, queue, mode string) (DeliveryContext, error) {
	consumer, err := s.newConsumer(ctx, queue)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = consumer.Close(closeCtx)
	}()
	waitCtx, cancel := context.WithTimeout(ctx, getWait)
	defer cancel()
	dc, err := consumer.Receive(waitCtx)
	if err != nil {
		// A pull from an empty queue surfaces as a context deadline.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil
		}
		return nil, classify10(err, s.password)
	}
	d := &delivery10{dc: dc, n: normalize10(dc.Message())}
	// Settle while the link is still open (the deferred Close above would
	// otherwise invalidate the delivery context).
	if err := settle(ctx, d, mode); err != nil {
		return nil, err
	}
	return d, nil
}

// receiver10 wraps a 1.0 consumer.
type receiver10 struct {
	consumer *rabbitmqamqp.Consumer
	password string
}

func (r *receiver10) Receive(ctx context.Context) (DeliveryContext, error) {
	dc, err := r.consumer.Receive(ctx)
	if err != nil {
		// Keep the deadline sentinel unwrapped so the consume loop can
		// end collection on timeout instead of failing.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, classify10(err, r.password)
	}
	return &delivery10{dc: dc, n: normalize10(dc.Message())}, nil
}

func (r *receiver10) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.consumer.Close(ctx)
}

func (s *session10) NewReceiver(ctx context.Context, queue string) (Receiver, error) {
	consumer, err := s.newConsumer(ctx, queue)
	if err != nil {
		return nil, err
	}
	return &receiver10{consumer: consumer, password: s.password}, nil
}
