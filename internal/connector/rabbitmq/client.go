// Package rabbitmq implements muxcat's RabbitMQ connector: a plain
// net/http client for the RabbitMQ Management HTTP API (HTTP basic auth
// with username + password). It covers broker overview, nodes, queues,
// exchanges, bindings, AMQP client connections, channels, consumers,
// streams, vhosts, health checks, and a raw request passthrough.
package rabbitmq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	connector.Register("rabbitmq", New)
}

// New builds the rabbitmq connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:     "rabbitmq",
		Aliases: []string{"rmq"},
		Short:   "RabbitMQ connector (Management HTTP API)",
		Long: `RabbitMQ connector over the Management HTTP API (HTTP basic
auth), a plain net/http client with no AMQP driver.

Quickstart:
  1. muxcat rabbitmq conn add local --url http://127.0.0.1:15672 --username guest --set-default
  2. muxcat rabbitmq overview
  3. muxcat rabbitmq queue ls

A connection carries credentials (username + password, encrypted at
rest, never echoed) and policies: readonly allows GET/HEAD requests
only. The command name is rabbitmq (alias rmq). The baseline is
RabbitMQ 3.8 through 4.x: responses are parsed leniently, and
version-specific endpoints (streams 3.9+, some health checks 4.x) are
requested directly — a 404 there maps to UNSUPPORTED_OPERATION with
the required minimum version. This connector talks to the Management
API only; use request to pass any other endpoint through.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newOverviewCmd(),
		newNodeCmd(),
		newQueueCmd(),
		newExchangeCmd(),
		newBindingCmd(),
		newConnectionCmd(),
		newChannelCmd(),
		newConsumerCmd(),
		newStreamCmd(),
		newVhostCmd(),
		newHealthCmd(),
		newRequestCmd(),
	)
	return c
}

// meta builds the envelope meta for rabbitmq commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "rabbitmq",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// client is an authenticated RabbitMQ Management API client. The password
// field holds the decrypted credential and lives in memory only; it is
// never written to any output channel.
type client struct {
	baseURL  string
	username string
	password string
	hc       *http.Client
}

// newClient builds a client from an instance + connection, decrypting the
// enc:v1: password blob when present.
func newClient(cfg *Config, conn Connection, timeout time.Duration) (*client, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	base, err := normalizeURL(inst.URL)
	if err != nil {
		return nil, err
	}
	c := &client{
		baseURL:  base,
		username: conn.Username,
		hc:       &http.Client{Timeout: timeout},
	}
	if conn.Password != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		plain, err := secret.Decrypt(key, conn.Password)
		if err != nil {
			return nil, err
		}
		c.password = string(plain)
	}
	return c, nil
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

// response is the raw carrier of an HTTP call.
type response struct {
	status  int
	headers map[string]string
	body    []byte
}

// endpointMinVersion maps version-specific endpoint prefixes to the
// minimum RabbitMQ version that provides them. A 404 on a matching path
// maps to UNSUPPORTED_OPERATION instead of a generic not-found.
var endpointMinVersion = []struct {
	prefix  string
	version string
}{
	{"/api/health/checks/is-in-service", "4.0"},
	{"/api/health/checks/ready-to-serve-clients", "4.0"},
	{"/api/deprecated-features", "3.13"},
	{"/api/stream/", "3.9"},
}

// minVersionOf returns the minimum RabbitMQ version for a path, or "".
func minVersionOf(path string) string {
	for _, e := range endpointMinVersion {
		if strings.HasPrefix(path, e.prefix) {
			return e.version
		}
	}
	return ""
}

// do performs one HTTP call and classifies non-2xx statuses into
// structured errors. path is appended to the instance base URL verbatim.
func (c *client) do(ctx context.Context, method, path string, body []byte) (*response, error) {
	r, err := c.exchange(ctx, method, path, body, "")
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status >= 300 {
		return r, classifyStatus(r.status, r.body, path)
	}
	return r, nil
}

// exchange performs one HTTP call without status classification: any
// completed exchange (including 4xx/5xx) is returned as-is for the request
// passthrough command. A nil body means no request body; a non-nil body
// defaults Content-Type to application/json unless contentType says
// otherwise ("" keeps the default).
func (c *client) exchange(ctx context.Context, method, path string, body []byte, contentType string) (*response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "invalid request path: "+path, "")
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, classifyHTTPErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, output.NewError(output.CodeQueryError, "failed to read response: "+err.Error(), "")
	}
	return &response{status: resp.StatusCode, headers: flattenHeaders(resp.Header), body: raw}, nil
}

// flattenHeaders joins multi-value headers per RFC 7230.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// classifyHTTPErr maps transport-level errors to muxcat error codes. The
// error text may contain the request URL but never credentials (basic auth
// travels in the header, and the base URL is validated userinfo-free).
func classifyHTTPErr(err error) *output.Error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
			return output.NewError(output.CodeTimeout,
				"request timed out: "+unwrapURL(urlErr), "increase --timeout or check the server")
		}
		low := strings.ToLower(urlErr.Err.Error())
		switch {
		case strings.Contains(low, "connection refused"),
			strings.Contains(low, "no such host"),
			strings.Contains(low, "connection reset"):
			return output.NewError(output.CodeConnectFailed,
				"failed to connect: "+unwrapURL(urlErr), "check the url of the instance and that the server is reachable")
		}
		var netErr net.Error
		if errors.As(urlErr.Err, &netErr) && netErr.Timeout() {
			return output.NewError(output.CodeTimeout,
				"request timed out: "+unwrapURL(urlErr), "increase --timeout or check the server")
		}
		return output.NewError(output.CodeConnectFailed, "request failed: "+unwrapURL(urlErr), "")
	}
	return output.NewError(output.CodeGeneral, "request failed: "+err.Error(), "")
}

// unwrapURL renders a url.Error without reserializing credentials; the op
// and URL are safe (the base URL carries no userinfo).
func unwrapURL(e *url.Error) string {
	return e.Op + " " + e.URL + ": " + e.Err.Error()
}

// apiErrorBody is RabbitMQ's structured error body: {"error":"...","reason":"..."}.
// error is usually a string but may be structured, so it stays untyped.
type apiErrorBody struct {
	Error  any    `json:"error"`
	Reason string `json:"reason"`
}

// classifyStatus maps a non-2xx status to a structured error. The
// {"error","reason"} body is parsed and reason rides in the hint. A 404 on
// a version-gated endpoint maps to UNSUPPORTED_OPERATION.
func classifyStatus(status int, body []byte, path string) *output.Error {
	var eb apiErrorBody
	_ = json.Unmarshal(body, &eb)
	reason := eb.Reason
	if reason == "" {
		reason = truncate(strings.TrimSpace(string(body)), 512)
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg := fmt.Sprintf("HTTP %d: %s", status, reason)
		return output.NewError(output.CodeAuthFailed, msg,
			"check the username/password of the connection; note the default guest/guest account can only log in from localhost")
	case http.StatusNotFound:
		if v := minVersionOf(path); v != "" {
			return output.NewError(output.CodeUnsupportedOperation,
				fmt.Sprintf("not found (HTTP %d): %s", status, path),
				fmt.Sprintf("this endpoint requires RabbitMQ >= %s (the connected server may be older)", v))
		}
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("not found (HTTP %d): %s", status, reason),
			"check the vhost and object name; vhost \"/\" is encoded as %2F")
	default:
		// The message carries the status (plus the server's error tag when
		// it is a plain string); reason goes to the hint only, so the same
		// text never appears twice.
		msg := fmt.Sprintf("HTTP %d", status)
		if es, ok := eb.Error.(string); ok && es != "" {
			msg = fmt.Sprintf("HTTP %d: %s", status, es)
		}
		hint := eb.Reason
		if hint == "" && eb.Error == nil {
			hint = truncate(strings.TrimSpace(string(body)), 512)
		}
		return output.NewError(output.CodeQueryError, msg, hint)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// esc path-escapes a vhost or object name for use in an API path.
// PathEscape already encodes "/" as %2F, so the default vhost "/" is
// handled correctly.
func esc(s string) string {
	return url.PathEscape(s)
}

// vhostFlag reads the --vhost flag, defaulting to "/".
func vhostFlag(cmd *cobra.Command) string {
	v := cli.FlagString(cmd, "vhost")
	if v == "" {
		return "/"
	}
	return v
}

// openForCmd resolves the connection for a data command and builds an
// authenticated client with the effective timeout.
func openForCmd(cmd *cobra.Command) (string, Connection, *client, time.Duration, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	cl, err := newClient(cfg, conn, timeout)
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	return name, conn, cl, timeout, nil
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

// decodeBody parses a response body as JSON into a generic value.
func decodeBody(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, output.NewError(output.CodeQueryError,
			"response is not valid JSON: "+err.Error(), "")
	}
	return v, nil
}

// decodeArray parses a response body as a JSON array of objects, keeping
// the raw value for JSONData. A non-array body is a QUERY_ERROR (e.g. the
// object shape pagination responses of 3.12+ are deliberately not used).
func decodeArray(raw []byte) ([]any, error) {
	v, err := decodeBody(raw)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, output.NewError(output.CodeQueryError,
			"response is not a JSON array", "the server answered with an unexpected shape")
	}
	return arr, nil
}

// applyLimit truncates rows to the global --limit (0 disables truncation).
func applyLimit(rows [][]any, limit int) ([][]any, bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// str reads a decoded JSON string field.
func str(v any) string {
	s, _ := v.(string)
	return s
}

// boolOf reads a decoded JSON boolean field.
func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// numOf reads a decoded JSON number field as int64.
func numOf(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// obj reads a decoded JSON object field.
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
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
