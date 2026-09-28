// Package mqtt implements muxcat's mqtt connector: an MQTT wire-protocol
// (data plane) client supporting both MQTT 3.1.1 (paho.mqtt.golang) and
// MQTT 5.0 (paho.golang). The protocol version is an instance attribute;
// the command surface is unified and dispatches to one of the two client
// implementations. It complements the emqx connector (Management HTTP
// API): manage with emqx, publish/subscribe with mqtt. Transports are
// TCP, TLS and WebSocket (ws/wss).
package mqtt

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	connector.Register("mqtt", New)
}

// New builds the mqtt connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "mqtt",
		Short: "MQTT connector (wire protocol, 3.1.1 and 5.0)",
		Long: `MQTT connector over the wire protocol (data plane), complementing
the emqx connector (Management HTTP API): manage with emqx,
publish/subscribe with mqtt.

Quickstart:
  1. muxcat mqtt conn add local --url mqtt://127.0.0.1:1883 --set-default
  2. muxcat mqtt conn test local
  3. muxcat mqtt sub events/# --count 1 &
     muxcat mqtt pub events/boot --payload hi

One connector speaks both MQTT 3.1.1 and MQTT 5.0: the protocol version
is an instance attribute (--protocol-version on conn add, default 3)
and each command dispatches to the matching client implementation.
Transports are TCP (mqtt://), TLS (mqtts://) and WebSocket (ws://,
wss://). sub is a one-shot batch: it collects --count messages (or
stops at --timeout) and returns one envelope; a streaming --follow
mode is not implemented. Every command opens a short-lived session
(clean start, random client ID).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newPubCmd(),
		newSubCmd(),
	)
	return c
}

// meta builds the envelope meta for mqtt commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "mqtt",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// queryTimeout resolves the command timeout: the connection-level timeout
// overrides the global --timeout.
func queryTimeout(conn Connection, flagTimeout time.Duration) (time.Duration, error) {
	if conn.Timeout == "" {
		return flagTimeout, nil
	}
	d, err := time.ParseDuration(conn.Timeout)
	if err != nil || d <= 0 {
		return 0, output.NewError(output.CodeConfigInvalid,
			"invalid connection timeout: "+conn.Timeout, "examples: 5s, 1m (must be > 0)")
	}
	return d, nil
}

// requireWritable refuses write operations on readonly connections.
func requireWritable(conn Connection, what string) error {
	if conn.Readonly {
		return output.NewError(output.CodeReadonlyViolation,
			what+" is not allowed on a readonly connection",
			"use a writable connection (-c), or recreate the connection without --readonly")
	}
	return nil
}

// decryptPassword decodes the enc:v1: password blob; "" stays "".
func decryptPassword(conn Connection) (string, error) {
	if conn.Password == "" {
		return "", nil
	}
	key, _, err := masterKey()
	if err != nil {
		return "", err
	}
	plain, err := secret.Decrypt(key, conn.Password)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// cmdEnv carries everything a data-plane command needs: the resolved
// connection, its instance, an open client, and a timeout-bound context.
type cmdEnv struct {
	name   string
	conn   Connection
	inst   Instance
	client Client
	ctx    context.Context
	cancel context.CancelFunc
}

// close releases the client and the context.
func (e *cmdEnv) close() {
	if e.client != nil {
		e.client.Close()
	}
	if e.cancel != nil {
		e.cancel()
	}
}

// openForCmd resolves the connection for a data command (from -c/--conn or
// the default), decrypts the password, and dials the broker. writeOp names
// a write operation (currently only "pub"): it is refused on readonly
// connections before any dialing happens; "" means a read-only command.
func openForCmd(cmd *cobra.Command, writeOp string) (*cmdEnv, error) {
	return openForConn(cmd, "", writeOp)
}

// openForConn is openForCmd with an explicit connection name (conn test
// takes it as a positional argument).
func openForConn(cmd *cobra.Command, connName, writeOp string) (*cmdEnv, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if connName == "" {
		connName = cli.FlagString(cmd, "conn")
	}
	name, conn, err := resolve(cfg, connName)
	if err != nil {
		return nil, err
	}
	if writeOp != "" {
		if err := requireWritable(conn, writeOp); err != nil {
			return nil, err
		}
	}
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return nil, err
	}
	if conn.TLSSkipVerify && !isTLSScheme(schemeOf(inst)) {
		return nil, output.NewError(output.CodeConfigInvalid,
			"tlsSkipVerify is set but the instance url is not mqtts:// or wss://",
			"tlsSkipVerify only makes sense with TLS; fix the url or remove tlsSkipVerify from "+FileName)
	}
	if conn.TLSSkipVerify {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"Warning: TLS certificate verification is disabled (tlsSkipVerify); the connection is not authenticated")
	}
	password, err := decryptPassword(conn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	cl, err := dial(ctx, inst, conn, password, timeout)
	if err != nil {
		cancel()
		return nil, err
	}
	return &cmdEnv{
		name: name, conn: conn, inst: inst, client: cl,
		ctx: ctx, cancel: cancel,
	}, nil
}

// tlsConfig builds the TLS config for mqtts/wss endpoints; nil for
// plaintext. tlsSkipVerify disables certificate verification (opt-in,
// warned about).
func tlsConfig(inst Instance, conn Connection) *tls.Config {
	if !isTLSScheme(schemeOf(inst)) {
		return nil
	}
	host := ""
	if u, err := url.Parse(inst.URL); err == nil {
		host = u.Hostname()
	}
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: conn.TLSSkipVerify, //nolint:gosec // explicit user opt-in with a stderr warning
		MinVersion:         tls.VersionTLS12,
	}
}

// isTLSScheme reports whether the scheme carries TLS (mqtts or wss).
func isTLSScheme(scheme string) bool {
	return scheme == "mqtts" || scheme == "wss"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
