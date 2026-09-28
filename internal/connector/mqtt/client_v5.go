package mqtt

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	"github.com/gorilla/websocket"
)

// clientV5 is the MQTT 5.0 client (paho.golang).
type clientV5 struct {
	c        *paho.Client
	router   *paho.StandardRouter
	password string
}

func dialV5(ctx context.Context, inst Instance, conn Connection, password string, timeout time.Duration) (Client, error) {
	u, err := url.Parse(inst.URL)
	if err != nil {
		return nil, classifyDial(err, password)
	}
	var nc net.Conn
	switch u.Scheme {
	case "mqtt":
		d := net.Dialer{Timeout: timeout}
		nc, err = d.DialContext(ctx, "tcp", u.Host)
	case "mqtts":
		d := tls.Dialer{Config: tlsConfig(inst, conn)}
		nc, err = d.DialContext(ctx, "tcp", u.Host)
		if err == nil {
			// tls.Conn is not thread-safe for writing.
			nc = packets.NewThreadSafeConn(nc)
		}
	case "ws", "wss":
		nc, err = dialWS(ctx, u, tlsConfig(inst, conn))
	default:
		err = fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err != nil {
		return nil, classifyDial(err, password)
	}
	id := clientID(conn)
	router := paho.NewStandardRouter()
	c := paho.NewClient(paho.ClientConfig{
		ClientID:      id,
		Conn:          nc,
		Router:        router,
		PacketTimeout: timeout,
	})
	ca, err := c.Connect(ctx, &paho.Connect{
		ClientID:     id,
		CleanStart:   true,
		KeepAlive:    30,
		Username:     conn.Username,
		UsernameFlag: conn.Username != "",
		Password:     []byte(password),
		PasswordFlag: password != "",
	})
	if err != nil {
		_ = nc.Close()
		return nil, classifyDialV5(err, ca, password)
	}
	return &clientV5{c: c, router: router, password: password}, nil
}

func (c *clientV5) Close() {
	_ = c.c.Disconnect(&paho.Disconnect{ReasonCode: 0})
}

func (c *clientV5) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	_, err := c.c.Publish(ctx, &paho.Publish{
		Topic:   topic,
		QoS:     qos,
		Retain:  retain,
		Payload: payload,
	})
	if err != nil {
		return classifyV5(err, c.password)
	}
	return nil
}

func (c *clientV5) Subscribe(ctx context.Context, filter string, qos byte, handler func(Message)) error {
	c.router.RegisterHandler(filter, func(p *paho.Publish) {
		handler(Message{Topic: p.Topic, Payload: p.Payload, QoS: p.QoS, Retained: p.Retain})
	})
	_, err := c.c.Subscribe(ctx, &paho.Subscribe{
		Subscriptions: []paho.SubscribeOptions{{Topic: filter, QoS: qos}},
	})
	if err != nil {
		return classifyV5(err, c.password)
	}
	return nil
}

// dialWS opens a WebSocket connection for the v5 client (ws/wss). The
// "mqtt" subprotocol is required by the spec.
func dialWS(ctx context.Context, u *url.URL, tc *tls.Config) (net.Conn, error) {
	d := *websocket.DefaultDialer
	d.TLSClientConfig = tc
	d.Subprotocols = []string{"mqtt"}
	ws, _, err := d.DialContext(ctx, u.String(), http.Header{})
	if err != nil {
		return nil, fmt.Errorf("websocket connection failed: %w", err)
	}
	return &wsConn{Conn: ws, Locker: &sync.Mutex{}}, nil
}

// wsConn adapts a gorilla/websocket connection to net.Conn (the same
// pattern paho's autopaho uses). The embedded sync.Locker makes writes
// thread-safe, as the paho packet writer requires.
type wsConn struct {
	*websocket.Conn
	r   io.Reader
	rio sync.Mutex
	sync.Locker
}

// SetDeadline sets both the read and write deadlines; deadlines are
// fatal in websocket connections.
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// Write writes one binary websocket message.
func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read reads across websocket message boundaries.
func (c *wsConn) Read(p []byte) (int, error) {
	c.rio.Lock()
	defer c.rio.Unlock()
	for {
		if c.r == nil {
			var err error
			if _, c.r, err = c.NextReader(); err != nil {
				return 0, err
			}
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
