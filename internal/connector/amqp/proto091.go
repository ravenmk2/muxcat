package amqp

import (
	"context"
	"fmt"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"

	"github.com/ravenmk2/muxcat/internal/output"
)

// session091 is the AMQP 0.9.1 session: one connection, one channel in
// confirm mode (publisher confirms double as the command's write path).
type session091 struct {
	conn     *amqp091.Connection
	ch       *amqp091.Channel
	returns  chan amqp091.Return
	password string
}

func dial091(ctx context.Context, inst Instance, conn Connection, vhost, password string, timeout time.Duration) (Session, error) {
	uri := dialURI(inst, conn, vhost, password)
	cfg := amqp091.Config{
		Dial:            amqp091.DefaultDial(timeout),
		TLSClientConfig: tlsConfig(inst, conn),
		Properties: amqp091.Table{
			"product": "muxcat",
		},
	}
	c, err := amqp091.DialConfig(uri, cfg)
	if err != nil {
		return nil, classifyDial(err, password, Protocol091, vhost)
	}
	ch, err := c.Channel()
	if err != nil {
		_ = c.Close()
		return nil, classify091(err, password)
	}
	s := &session091{conn: c, ch: ch, password: password, returns: make(chan amqp091.Return, 16)}
	if err := ch.Confirm(false); err != nil {
		_ = c.Close()
		return nil, classify091(err, password)
	}
	ch.NotifyReturn(s.returns)
	return s, nil
}

func (s *session091) Close() {
	_ = s.ch.Close()
	_ = s.conn.Close()
}

func (s *session091) ServerInfo() (product, version string) {
	product, _ = s.conn.Properties["product"].(string)
	version, _ = s.conn.Properties["version"].(string)
	return product, version
}

func (s *session091) DeclareQueue(_ context.Context, spec QueueSpec) error {
	args := amqp091.Table{}
	for k, v := range spec.Args {
		args[k] = v
	}
	if spec.Type != "" {
		args["x-queue-type"] = spec.Type
	}
	_, err := s.ch.QueueDeclare(spec.Name, spec.Durable, spec.AutoDelete, spec.Exclusive, false, args)
	if err != nil {
		return classify091(err, s.password)
	}
	return nil
}

func (s *session091) DeleteQueue(_ context.Context, name string, ifEmpty, ifUnused bool) error {
	_, err := s.ch.QueueDelete(name, ifUnused, ifEmpty, false)
	if err != nil {
		return classify091(err, s.password)
	}
	return nil
}

func (s *session091) PurgeQueue(_ context.Context, name string) (int, error) {
	n, err := s.ch.QueuePurge(name, false)
	if err != nil {
		return 0, classify091(err, s.password)
	}
	return n, nil
}

func (s *session091) QueueInfo(_ context.Context, name string) (*QueueInfo, error) {
	// Passive declare doubles as the existence probe.
	q, err := s.ch.QueueDeclarePassive(name, false, false, false, false, nil)
	if err != nil {
		return nil, classify091(err, s.password)
	}
	return &QueueInfo{Name: q.Name, Messages: int64(q.Messages), Consumers: int64(q.Consumers)}, nil
}

func (s *session091) DeclareExchange(_ context.Context, spec ExchangeSpec) error {
	args := amqp091.Table{}
	for k, v := range spec.Args {
		args[k] = v
	}
	err := s.ch.ExchangeDeclare(spec.Name, spec.Type, spec.Durable, spec.AutoDelete, false, false, args)
	if err != nil {
		return classify091(err, s.password)
	}
	return nil
}

func (s *session091) DeleteExchange(_ context.Context, name string, ifUnused bool) error {
	if err := s.ch.ExchangeDelete(name, ifUnused, false); err != nil {
		return classify091(err, s.password)
	}
	return nil
}

func (s *session091) ExchangeInfo(_ context.Context, name string) (*ExchangeInfo, error) {
	// Passive declare only checks existence; the other parameters are
	// ignored by the broker. The 0.9.1 probe cannot learn the type.
	if err := s.ch.ExchangeDeclarePassive(name, "direct", false, false, false, false, nil); err != nil {
		return nil, classify091(err, s.password)
	}
	return &ExchangeInfo{Name: name}, nil
}

func (s *session091) Bind(_ context.Context, exchange, queue, routingKey string, args map[string]any) (string, error) {
	t := amqp091.Table{}
	for k, v := range args {
		t[k] = v
	}
	if err := s.ch.QueueBind(queue, routingKey, exchange, false, t); err != nil {
		return "", classify091(err, s.password)
	}
	return "", nil
}

func (s *session091) Unbind(_ context.Context, exchange, queue, routingKey string) error {
	if err := s.ch.QueueUnbind(queue, routingKey, exchange, nil); err != nil {
		return classify091(err, s.password)
	}
	return nil
}

// publishing091 maps the normalized Outgoing to an amqp091.Publishing.
func publishing091(msg Outgoing) amqp091.Publishing {
	p := amqp091.Publishing{
		Body:    msg.Body,
		Headers: amqp091.Table{},
	}
	if msg.Persistent {
		p.DeliveryMode = 2
	}
	for k, v := range msg.Headers {
		p.Headers[k] = v
	}
	if len(p.Headers) == 0 {
		p.Headers = nil
	}
	props := msg.Props
	p.MessageId = propString(props, "message_id")
	p.CorrelationId = propString(props, "correlation_id")
	p.ContentType = propString(props, "content_type")
	p.ContentEncoding = propString(props, "content_encoding")
	p.ReplyTo = propString(props, "reply_to")
	p.Type = propString(props, "type")
	p.Expiration = propString(props, "expiration")
	p.UserId = propString(props, "user_id")
	p.AppId = propString(props, "app_id")
	if f, ok := props["priority"].(float64); ok {
		p.Priority = uint8(f)
	}
	if t, ok := propTime(props); ok {
		p.Timestamp = t
	}
	return p
}

func (s *session091) Publish(ctx context.Context, exchange, routingKey string, msg Outgoing) (PublishOutcome, error) {
	var oc PublishOutcome
	dc, err := s.ch.PublishWithDeferredConfirmWithContext(ctx, exchange, routingKey, true, false, publishing091(msg))
	if err != nil {
		return oc, classify091(err, s.password)
	}
	if dc == nil {
		return oc, output.NewError(output.CodeGeneral, "publisher confirms unavailable", "")
	}
	acked, err := dc.WaitContext(ctx)
	if err != nil {
		return oc, classify091(err, s.password)
	}
	oc.Confirmed = acked
	// A mandatory return is dispatched before the confirm on the same
	// connection, so by the time the confirm lands the return has been
	// delivered to the channel.
	select {
	case <-s.returns:
		oc.Unroutable = true
	default:
	}
	return oc, nil
}

// delivery091 adapts an amqp091.Delivery to DeliveryContext.
type delivery091 struct {
	d amqp091.Delivery
	n *Delivery
}

func (dc *delivery091) Delivery() *Delivery { return dc.n }

func (dc *delivery091) Ack(_ context.Context) error     { return dc.d.Ack(false) }
func (dc *delivery091) Requeue(_ context.Context) error { return dc.d.Nack(false, true) }
func (dc *delivery091) Reject(_ context.Context) error  { return dc.d.Reject(false) }

// normalize091 converts an amqp091.Delivery to the normalized Delivery.
func normalize091(d amqp091.Delivery) *Delivery {
	props := map[string]any{}
	putStr := func(key, val string) {
		if val != "" {
			props[key] = val
		}
	}
	putStr("content_type", d.ContentType)
	putStr("content_encoding", d.ContentEncoding)
	putStr("message_id", d.MessageId)
	putStr("correlation_id", d.CorrelationId)
	putStr("reply_to", d.ReplyTo)
	putStr("type", d.Type)
	putStr("expiration", d.Expiration)
	putStr("user_id", d.UserId)
	putStr("app_id", d.AppId)
	if d.DeliveryMode > 0 {
		props["delivery_mode"] = d.DeliveryMode
	}
	if d.Priority > 0 {
		props["priority"] = d.Priority
	}
	if !d.Timestamp.IsZero() {
		props["timestamp"] = d.Timestamp.Format(time.RFC3339)
	}
	if len(d.Headers) > 0 {
		h := map[string]any{}
		for k, v := range d.Headers {
			h[k] = v
		}
		props["headers"] = h
	}
	return &Delivery{
		Exchange:    d.Exchange,
		RoutingKey:  d.RoutingKey,
		Redelivered: d.Redelivered,
		Payload:     d.Body,
		Properties:  props,
	}
}

func (s *session091) Get(ctx context.Context, queue, mode string) (DeliveryContext, error) {
	d, ok, err := s.ch.Get(queue, false)
	if err != nil {
		return nil, classify091(err, s.password)
	}
	if !ok {
		return nil, nil
	}
	dc := &delivery091{d: d, n: normalize091(d)}
	if err := settle(ctx, dc, mode); err != nil {
		return nil, err
	}
	return dc, nil
}

// receiver091 consumes deliveries pushed by the broker.
type receiver091 struct {
	ch         *amqp091.Channel
	tag        string
	deliveries <-chan amqp091.Delivery
}

func (r *receiver091) Receive(ctx context.Context) (DeliveryContext, error) {
	select {
	case d, ok := <-r.deliveries:
		if !ok {
			return nil, output.NewError(output.CodeQueryError, "consumer cancelled by the broker", "")
		}
		return &delivery091{d: d, n: normalize091(d)}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *receiver091) Close() error { return r.ch.Cancel(r.tag, false) }

func (s *session091) NewReceiver(ctx context.Context, queue string) (Receiver, error) {
	tag := fmt.Sprintf("muxcat-%d", time.Now().UnixNano())
	deliveries, err := s.ch.ConsumeWithContext(ctx, queue, tag, false, false, false, false, nil)
	if err != nil {
		return nil, classify091(err, s.password)
	}
	return &receiver091{ch: s.ch, tag: tag, deliveries: deliveries}, nil
}
