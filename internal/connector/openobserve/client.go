// Package openobserve implements muxcat's OpenObserve connector: a plain
// net/http client for OpenObserve's REST API (HTTP basic auth). Streams,
// SQL query (with around/values), log ingestion, and raw request
// passthrough are covered; metrics/traces ingestion and admin APIs
// (users/functions/metrics) are out of scope.
package openobserve

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

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

func init() {
	connector.Register("openobserve", New)
}

// New builds the openobserve connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:     "openobserve",
		Aliases: []string{"o2"},
		Short:   "OpenObserve connector",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newStreamCmd(),
		newQueryCmd(),
		newIngestCmd(),
		newRequestCmd(),
		newApiCmd(),
	)
	return c
}

// meta builds the envelope meta for openobserve commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "openobserve",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// client is an authenticated OpenObserve HTTP client. The password field
// holds the decrypted credential and lives in memory only; it is never
// written to any output channel.
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

// do performs one HTTP call and classifies non-2xx statuses into
// structured errors. path is appended to the instance base URL verbatim
// (it carries the /api/{org}/... prefix).
func (c *client) do(ctx context.Context, method, path string, body []byte) (*response, error) {
	r, err := c.exchange(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status >= 300 {
		return r, classifyStatus(r.status, r.body)
	}
	return r, nil
}

// exchange performs one HTTP call without status classification: any
// completed exchange (including 4xx/5xx) is returned as-is for the request
// passthrough command. A nil body means no request body; a non-nil body
// defaults Content-Type to application/json.
func (c *client) exchange(ctx context.Context, method, path string, body []byte) (*response, error) {
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
		req.Header.Set("Content-Type", "application/json")
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

// apiError is OpenObserve's structured error body. hint and suggestions
// carry the server's self-correcting guidance and are passed through.
type apiError struct {
	Code        int      `json:"code"`
	Message     string   `json:"message"`
	ErrorDetail string   `json:"error_detail"`
	Hint        string   `json:"hint"`
	Suggestions []string `json:"suggestions"`
}

// classifyStatus maps a non-2xx status to a structured error, passing the
// server's hint/suggestions through for AI/agent callers.
func classifyStatus(status int, body []byte) *output.Error {
	var ae apiError
	_ = json.Unmarshal(body, &ae)
	msg := ae.Message
	if msg == "" {
		msg = ae.ErrorDetail
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 512))
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return output.NewError(output.CodeAuthFailed, msg,
			"check the username/password of the connection")
	}
	hint := ae.Hint
	if len(ae.Suggestions) > 0 {
		s := "suggestions: " + strings.Join(ae.Suggestions, ", ")
		if hint != "" {
			hint += "; " + s
		} else {
			hint = s
		}
	}
	return output.NewError(output.CodeQueryError, msg, hint)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
