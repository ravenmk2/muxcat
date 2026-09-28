// Package amqp implements muxcat's amqp connector: an AMQP wire-protocol
// (data plane) client supporting both AMQP 0.9.1 (amqp091-go) and AMQP 1.0
// (rabbitmq-amqp-go-client). The protocol is an instance attribute; the
// command surface is unified and dispatches to one of the two client
// implementations. It complements the rabbitmq connector (Management HTTP
// API): manage with rmq, publish/consume with amqp.
package amqp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
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
	connector.Register("amqp", New)
}

// New builds the amqp connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "amqp",
		Short: "AMQP connector (wire protocol, 0.9.1 and 1.0)",
		Long: `AMQP connector over the wire protocol (data plane), complementing
the rabbitmq connector (Management HTTP API): manage with rmq,
publish/consume with amqp.

Quickstart:
  1. muxcat amqp conn add local --url amqp://127.0.0.1:5672 --username guest --password guest --set-default
  2. muxcat amqp conn test local
  3. muxcat amqp queue declare q1 && muxcat amqp exchange publish amq.direct --routing-key q1 --payload hi

One connector speaks both AMQP 0.9.1 and AMQP 1.0: the protocol is an
instance attribute (--protocol on conn add, default 1.0; AMQP 1.0
requires RabbitMQ >= 4.0) and each command dispatches to the matching
client implementation. The vhost is a connection attribute, overridable
per command with --vhost. Neither protocol has a list primitive, so
there is no queue/exchange/binding ls — use rmq (Management API) for
listings. Every command opens a short-lived session.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newQueueCmd(),
		newExchangeCmd(),
		newBindingCmd(),
		newConsumeCmd(),
	)
	return c
}

// meta builds the envelope meta for amqp commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "amqp",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// addVhostFlag registers the unified --vhost override flag.
func addVhostFlag(c *cobra.Command) {
	c.Flags().String("vhost", "", "vhost (overrides connection default)")
}

// vhostOf resolves the effective vhost: --vhost > connection vhost > "/".
func vhostOf(cmd *cobra.Command, conn Connection) string {
	if cmd.Flags().Changed("vhost") {
		return cli.FlagString(cmd, "vhost")
	}
	if conn.Vhost != "" {
		return conn.Vhost
	}
	return "/"
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
// connection, its instance, an open session, and a timeout-bound context.
type cmdEnv struct {
	name    string
	conn    Connection
	inst    Instance
	session Session
	vhost   string
	ctx     context.Context
	cancel  context.CancelFunc
}

// close releases the session and the context.
func (e *cmdEnv) close() {
	if e.session != nil {
		e.session.Close()
	}
	if e.cancel != nil {
		e.cancel()
	}
}

// openForCmd resolves the connection for a data command (from -c/--conn or
// the default), decrypts the password, and opens a session against the
// effective vhost. writeOp names a write operation (e.g. "queue declare"):
// it is refused on readonly connections before any dialing happens; ""
// means a read-only command.
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
	if err != nil {
		return nil, err
	}
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return nil, err
	}
	if conn.TLSSkipVerify && schemeOf(inst) != "amqps" {
		return nil, output.NewError(output.CodeConfigInvalid,
			"tlsSkipVerify is set but the instance url is not amqps://",
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
	vhost := vhostOf(cmd, conn)
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	s, err := openSession(ctx, inst, conn, vhost, password, timeout)
	if err != nil {
		cancel()
		return nil, err
	}
	return &cmdEnv{
		name: name, conn: conn, inst: inst, session: s, vhost: vhost,
		ctx: ctx, cancel: cancel,
	}, nil
}

// tlsConfig builds the TLS config for amqps endpoints; nil for plaintext.
// tlsSkipVerify disables certificate verification (opt-in, warned about).
func tlsConfig(inst Instance, conn Connection) *tls.Config {
	if schemeOf(inst) != "amqps" {
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

// parseJSONArg parses a --args/--props JSON object flag; "" yields an
// empty map.
func parseJSONArg(flag, raw string) (map[string]any, error) {
	out := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("invalid %s value: %v", flag, err), "expected a JSON object, e.g. '{\"x-queue-type\":\"quorum\"}'")
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
