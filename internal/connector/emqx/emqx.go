// Package emqx implements muxcat's EMQX connector: a plain net/http
// client for the EMQX 5.x HTTP API (/api/v5, verified up to 5.8.6, the
// last open-source release). A connection carries two credential pairs —
// a dashboard username/password and/or an API key/secret, at least one
// complete pair — and the client routes authentication per request: the
// API key pair authenticates with HTTP Basic and is preferred (no login
// round-trip); the dashboard pair lazily exchanges for a bearer JWT via
// POST /api/v5/login, cached in memory only and refreshed once on 401.
package emqx

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
	"strconv"
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
	connector.Register("emqx", New)
}

// New builds the emqx connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "emqx",
		Short: "EMQX connector (5.x HTTP API)",
		Long: `EMQX connector over the EMQX 5.x HTTP API (/api/v5, a plain
net/http client, no external driver), verified against 5.8.6 (the last
open-source release).

Quickstart:
  1. muxcat emqx conn add local --url http://127.0.0.1:18083 --username admin --set-default
  2. muxcat emqx status
  3. muxcat emqx client ls

A connection carries two credential pairs — a dashboard username/password
and/or an API key/secret; at least one complete pair is required. The
client routes authentication per request: with an API key pair present,
requests use HTTP Basic (no login round-trip, preferred); otherwise the
dashboard pair lazily exchanges for a bearer JWT (POST /api/v5/login,
cached in memory only, re-login once on 401). password and apiSecret are
stored encrypted (enc:v1:) and never echoed. readonly connections block
write operations (pub, client kick, banned add/rm, retained rm).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newStatusCmd(),
		newNodeCmd(),
		newListenerCmd(),
		newClientCmd(),
		newSubCmd(),
		newTopicCmd(),
		newPubCmd(),
		newMetricCmd(),
		newAlarmCmd(),
		newBannedCmd(),
		newRetainedCmd(),
	)
	return c
}

// meta builds the envelope meta for emqx commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "emqx",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// client is an EMQX HTTP client. The password, the apiSecret and the
// login token live in memory only; none is ever written to any output
// channel.
type client struct {
	baseURL     string
	username    string
	password    string
	apiKey      string
	apiSecret   string
	hc          *http.Client
	token       string // dashboard JWT, in memory only
	tokenExpiry time.Time
	version     string // filled by login / conn test, informational
}

// newClient builds a client from an instance + connection, decrypting the
// enc:v1: password and apiSecret blobs when present.
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
		apiKey:   conn.APIKey,
		hc:       &http.Client{},
	}
	if timeout > 0 {
		c.hc.Timeout = timeout
	}
	if conn.Password != "" {
		plain, err := decryptSecret(conn.Password)
		if err != nil {
			return nil, err
		}
		c.password = plain
	}
	if conn.APISecret != "" {
		plain, err := decryptSecret(conn.APISecret)
		if err != nil {
			return nil, err
		}
		c.apiSecret = plain
	}
	return c, nil
}

// decryptSecret decrypts an enc:v1: blob (password or apiSecret); the
// plaintext stays in memory only.
func decryptSecret(blob string) (string, error) {
	key, _, err := masterKey()
	if err != nil {
		return "", err
	}
	plain, err := secret.Decrypt(key, blob)
	if err != nil {
		return "", err
	}
	return string(plain), nil
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
	status int
	body   []byte
}

func (c *client) hasAPIKey() bool    { return c.apiKey != "" && c.apiSecret != "" }
func (c *client) hasDashboard() bool { return c.username != "" && c.password != "" }

// login exchanges the dashboard username/password for a bearer JWT via
// POST /api/v5/login. The token lives in memory only.
func (c *client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	r, err := c.requestAuth(ctx, http.MethodPost, "/api/v5/login", nil, body, nil)
	if err != nil {
		return err
	}
	if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
		return output.NewError(output.CodeAuthFailed,
			fmt.Sprintf("login failed (HTTP %d): %s", r.status, errDetail(r.body)),
			"check the connection's username/password")
	}
	if r.status < 200 || r.status >= 300 {
		return classifyStatus(r.status, r.body)
	}
	var v struct {
		Token   string `json:"token"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(r.body, &v); err != nil || v.Token == "" {
		return output.NewError(output.CodeQueryError,
			"login response has no token: "+truncate(string(r.body), 512), "")
	}
	c.token = v.Token
	c.version = v.Version
	// EMQX dashboard tokens default to a 1h TTL; refresh ahead of it.
	c.tokenExpiry = time.Now().Add(55 * time.Minute)
	return nil
}

// send performs one API call, routing authentication per request: with an
// API key pair present, the request uses HTTP Basic (no login round-trip,
// preferred); otherwise the dashboard pair lazily logs in for a bearer
// JWT, re-logging in once on 401. requireDashboard (reserved, unused by
// the current command set) forces the JWT path and fails with AUTH_FAILED
// when no dashboard credentials are configured. A non-2xx status is
// classified into a structured error.
func (c *client) send(ctx context.Context, method, path string, params url.Values, body []byte, requireDashboard bool) (*response, error) {
	if requireDashboard || !c.hasAPIKey() {
		if !c.hasDashboard() {
			return nil, output.NewError(output.CodeAuthFailed,
				"this command requires dashboard credentials (username/password)",
				"recreate the connection with muxcat emqx conn add <name> --username u --password p")
		}
		return c.sendBearer(ctx, method, path, params, body)
	}
	return c.sendBasic(ctx, method, path, params, body)
}

// sendBasic performs one API call with API key Basic auth on every
// request.
func (c *client) sendBasic(ctx context.Context, method, path string, params url.Values, body []byte) (*response, error) {
	r, err := c.requestAuth(ctx, method, path, params, body, func(req *http.Request) {
		req.SetBasicAuth(c.apiKey, c.apiSecret)
	})
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status >= 300 {
		if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
			return r, output.NewError(output.CodeAuthFailed,
				fmt.Sprintf("HTTP %d: %s", r.status, errDetail(r.body)),
				"check the connection's apiKey/apiSecret (a complete API key pair)")
		}
		return r, classifyStatus(r.status, r.body)
	}
	return r, nil
}

// sendBearer performs one API call with a dashboard bearer JWT, logging
// in lazily and re-logging in once when the server rejects the token.
func (c *client) sendBearer(ctx context.Context, method, path string, params url.Values, body []byte) (*response, error) {
	attach := func(req *http.Request) {
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
	}
	if c.token == "" || time.Now().After(c.tokenExpiry) {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}
	r, err := c.requestAuth(ctx, method, path, params, body, attach)
	if err != nil {
		return nil, err
	}
	if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
		c.token = ""
		if lerr := c.login(ctx); lerr == nil {
			r, err = c.requestAuth(ctx, method, path, params, body, attach)
			if err != nil {
				return nil, err
			}
		}
	}
	if r.status < 200 || r.status >= 300 {
		if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
			return r, output.NewError(output.CodeAuthFailed,
				fmt.Sprintf("HTTP %d: %s", r.status, errDetail(r.body)),
				"check the connection's username/password (dashboard account)")
		}
		return r, classifyStatus(r.status, r.body)
	}
	return r, nil
}

// requestAuth performs the raw HTTP call, letting attach set the
// authentication header. params travel as the query string; body is a
// JSON payload when non-nil.
func (c *client) requestAuth(ctx context.Context, method, path string, params url.Values, body []byte, attach func(*http.Request)) (*response, error) {
	u := c.baseURL + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "invalid request: "+err.Error(), "")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if attach != nil {
		attach(req)
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
	return &response{status: resp.StatusCode, body: raw}, nil
}

// classifyHTTPErr maps transport-level errors to muxcat error codes. The
// error text may contain the request URL but never credentials (they
// travel in headers, and the base URL is validated userinfo-free).
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

// classifyStatus maps a non-2xx status to a structured error. 401/403 is
// normally handled by the send path with a credential-specific hint; this
// is the fallback (e.g. the login endpoint itself).
func classifyStatus(status int, body []byte) *output.Error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return output.NewError(output.CodeAuthFailed,
			fmt.Sprintf("HTTP %d: %s", status, errDetail(body)),
			"check the connection's credentials (dashboard username/password or apiKey/apiSecret)")
	case http.StatusNotFound:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("not found (HTTP %d): %s", status, errDetail(body)),
			"check the addressing (node name, clientid, topic, banned as/who)")
	default:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("HTTP %d: %s", status, errDetail(body)), "")
	}
}

// errDetail extracts a human-readable message from EMQX's error body
// shape {code, message}; the response body is truncated to 512 characters
// to keep the message readable.
func errDetail(body []byte) string {
	var v map[string]any
	if json.Unmarshal(body, &v) == nil {
		if m, ok := v["message"].(string); ok && m != "" {
			return truncate(m, 512)
		}
		if e, ok := v["error"].(string); ok && e != "" {
			return truncate(e, 512)
		}
	}
	return truncate(string(body), 512)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// decodeJSON parses an EMQX response body into v (list or map); a bare
// non-JSON body becomes a QUERY_ERROR.
func decodeJSON(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return output.NewError(output.CodeQueryError,
			"unexpected response body: "+truncate(string(body), 512), "")
	}
	return nil
}

// fetchPages fetches a paginated EMQX listing ({data, meta:{hasnext}}):
// each page requests min(remaining, 100) rows and paging follows
// meta.hasnext until the global --limit; truncated reports whether more
// rows remain on the server. limit <= 0 fetches all rows.
func (c *client) fetchPages(ctx context.Context, path string, params url.Values, limit int) ([]map[string]any, bool, error) {
	items := []map[string]any{}
	page := 1
	for {
		pageLimit := 100
		if limit > 0 {
			remaining := limit - len(items)
			if remaining <= 0 {
				return items, true, nil
			}
			if remaining < pageLimit {
				pageLimit = remaining
			}
		}
		p := url.Values{}
		for k, vs := range params {
			p[k] = vs
		}
		p.Set("page", strconv.Itoa(page))
		p.Set("limit", strconv.Itoa(pageLimit))
		r, err := c.send(ctx, http.MethodGet, path, p, nil, false)
		if err != nil {
			return nil, false, err
		}
		var v struct {
			Data []json.RawMessage `json:"data"`
			Meta struct {
				HasNext bool `json:"hasnext"`
			} `json:"meta"`
		}
		if err := decodeJSON(r.body, &v); err != nil {
			return nil, false, err
		}
		for _, raw := range v.Data {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, false, output.NewError(output.CodeQueryError,
					"unexpected list item: "+truncate(string(raw), 512), "")
			}
			items = append(items, m)
		}
		if !v.Meta.HasNext {
			return items, false, nil
		}
		if limit > 0 && len(items) >= limit {
			return items[:limit], true, nil
		}
		page++
	}
}

// applyLimit truncates rows to the global --limit (0 disables truncation).
func applyLimit(rows [][]any, limit int) ([][]any, bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// strOf reads a string field tolerantly.
func strOf(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// intOf reads a numeric field tolerantly (JSON numbers decode as float64).
func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch n := m[k].(type) {
		case float64:
			return int(n)
		case string:
			var v int
			if _, err := fmt.Sscan(n, &v); err == nil {
				return v
			}
		}
	}
	return 0
}

// fmtUptime renders EMQX's millisecond uptime as a compact duration.
func fmtUptime(ms int) string {
	if ms <= 0 {
		return ""
	}
	return (time.Duration(ms) * time.Millisecond).Truncate(time.Second).String()
}

// openForCmd resolves the connection for a data command and builds an
// authenticated client with the effective timeout.
func openForCmd(cmd *cobra.Command) (string, Connection, *client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", Connection{}, nil, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
		return "", Connection{}, nil, err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return "", Connection{}, nil, err
	}
	cl, err := newClient(cfg, conn, timeout)
	if err != nil {
		return "", Connection{}, nil, err
	}
	return name, conn, cl, nil
}
