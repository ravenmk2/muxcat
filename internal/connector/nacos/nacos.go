// Package nacos implements muxcat's Nacos connector: a plain net/http
// client for Nacos 2.x and 3.x (config, service discovery and namespace
// APIs). The server major version is pinned per instance or auto-detected
// via the 3.x public state endpoint; an internal v2/v3 API adapter keeps
// the command layer version-free. Auth is username/password login
// exchanging for a bearer token, cached in memory only.
package nacos

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
	connector.Register("nacos", New)
}

// New builds the nacos connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:   "nacos",
		Short: "Nacos connector (2.x and 3.x)",
		Long: `Nacos connector over the HTTP OpenAPI (a plain net/http client, no
external driver), supporting both Nacos 2.x and 3.x servers.

Quickstart:
  1. muxcat nacos conn add local --url http://127.0.0.1:8848 --username nacos --set-default
  2. muxcat nacos config ls
  3. muxcat nacos service ls

The url is the server root (no /nacos context path). The server major
version is auto-detected per instance (or pinned with conn add
--version 2|3). A connection carries optional credentials (username +
password, encrypted at rest, never echoed; both 2.x with auth disabled
and 3.x client-side reads work anonymously), a working namespace
(default public, overridable per command with --namespace or its --ns
alias) and policies: readonly blocks config publish/delete. Beyond
reads, the connector covers config publish/delete and a config watch
that follows changes (long-polling on 2.x, md5 polling on 3.x).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newConfigCmd(),
		newServiceCmd(),
		newInstanceCmd(),
		newNamespaceCmd(),
	)
	return c
}

// meta builds the envelope meta for nacos commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "nacos",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// client is a Nacos HTTP client. The password and the login token live in
// memory only; neither is ever written to any output channel.
type client struct {
	baseURL     string
	versionPin  string
	username    string
	password    string
	hc          *http.Client
	version     int // resolved major version: 2 or 3 (0 = unresolved)
	token       string
	tokenExpiry time.Time
}

// newClient builds a client from an instance + connection, decrypting the
// enc:v1: password blob when present. A zero timeout disables the
// client-level deadline (watch long-polling; the transport's own dial
// timeout still applies).
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
		baseURL:    base,
		versionPin: inst.versionPin(),
		username:   conn.Username,
		hc:         &http.Client{},
	}
	if timeout > 0 {
		c.hc.Timeout = timeout
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
	status int
	body   []byte
}

func (c *client) hasCreds() bool { return c.username != "" && c.password != "" }

// ensureReady resolves the server major version and logs in when the
// connection carries credentials. An expired token (tokenTtl, with a
// safety margin) is refreshed transparently.
func (c *client) ensureReady(ctx context.Context) error {
	if c.version == 0 {
		v, err := c.resolveVersion(ctx)
		if err != nil {
			return err
		}
		c.version = v
	}
	if c.hasCreds() && (c.token == "" || time.Now().After(c.tokenExpiry)) {
		return c.login(ctx)
	}
	return nil
}

// login exchanges username/password for a bearer token. Both versions
// answer {accessToken, tokenTtl, globalAdmin, username}; only the path
// differs. The token lives in memory only.
func (c *client) login(ctx context.Context) error {
	path := "/nacos/v1/auth/login"
	if c.version == 3 {
		path = "/nacos/v3/auth/user/login"
	}
	form := url.Values{"username": {c.username}, "password": {c.password}}
	r, err := c.request(ctx, http.MethodPost, path, form, nil, nil)
	if err != nil {
		return err
	}
	if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
		// Both versions answer a wrong password with 403 and the
		// misleading "User not found!" text, so classify by status only.
		return output.NewError(output.CodeAuthFailed,
			fmt.Sprintf("login failed (HTTP %d): %s", r.status, errDetail(r.body)),
			"check the connection's username/password")
	}
	if r.status < 200 || r.status >= 300 {
		return classifyStatus(r.status, r.body)
	}
	var v struct {
		AccessToken string `json:"accessToken"`
		TokenTTL    int64  `json:"tokenTtl"`
	}
	if err := json.Unmarshal(r.body, &v); err != nil || v.AccessToken == "" {
		return output.NewError(output.CodeQueryError,
			"login response has no accessToken: "+truncate(string(r.body), 512), "")
	}
	c.token = v.AccessToken
	ttl := v.TokenTTL
	if ttl <= 0 {
		ttl = 3600
	}
	// Refresh ahead of the server-side expiry.
	c.tokenExpiry = time.Now().Add(time.Duration(ttl)*time.Second - 30*time.Second)
	return nil
}

// send performs one API call: version resolution and login first, the
// bearer token attached, and a non-2xx status classified into a structured
// error. On 401/403 with configured credentials the token is refreshed
// once and the call retried.
func (c *client) send(ctx context.Context, method, path string, params url.Values, body []byte, headers map[string]string) (*response, error) {
	if err := c.ensureReady(ctx); err != nil {
		return nil, err
	}
	r, err := c.request(ctx, method, path, params, body, headers)
	if err != nil {
		return nil, err
	}
	if (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden) && c.hasCreds() {
		c.token = ""
		if lerr := c.login(ctx); lerr == nil {
			r, err = c.request(ctx, method, path, params, body, headers)
			if err != nil {
				return nil, err
			}
		}
	}
	if r.status < 200 || r.status >= 300 {
		return r, classifyStatus(r.status, r.body)
	}
	return r, nil
}

// request performs the raw HTTP call. params travel as the query string
// for GET/DELETE and as a form body otherwise; a non-nil body overrides
// the form body (params stay in the query).
func (c *client) request(ctx context.Context, method, path string, params url.Values, body []byte, headers map[string]string) (*response, error) {
	u := c.baseURL + path
	var rdr io.Reader
	contentType := ""
	switch {
	case body != nil:
		rdr = bytes.NewReader(body)
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	case method == http.MethodGet || method == http.MethodDelete:
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	default:
		if len(params) > 0 {
			rdr = strings.NewReader(params.Encode())
			contentType = "application/x-www-form-urlencoded"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "invalid request: "+err.Error(), "")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
// error text may contain the request URL but never credentials (the token
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

// classifyStatus maps a non-2xx status to a structured error.
func classifyStatus(status int, body []byte) *output.Error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return output.NewError(output.CodeAuthFailed,
			fmt.Sprintf("HTTP %d: %s", status, errDetail(body)),
			"check the connection's username/password (Nacos 3.x admin APIs reject anonymous access; 2.x answers a wrong password with a misleading \"User not found!\")")
	case http.StatusNotFound:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("not found (HTTP %d): %s", status, errDetail(body)),
			"check the addressing triple: dataId, group (-g, default DEFAULT_GROUP) and namespace (--namespace, default public)")
	default:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("HTTP %d: %s", status, errDetail(body)), "")
	}
}

// errDetail extracts a human-readable message from Nacos' two error body
// shapes: the Spring standard error JSON {timestamp,status,error,message}
// and the Nacos envelope {code,message,data}. The response body is
// truncated to 512 characters to keep the message readable.
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

// decodeData parses a Nacos response body. Bodies in envelope form
// {code,message,data} are unwrapped (a non-zero code becomes a
// QUERY_ERROR carrying the envelope message); anything else is returned
// as parsed JSON, falling back to the raw text (e.g. a v1 config body).
// Success codes: 0 (v2/v3 envelopes) and 200 (some v1 console endpoints).
func decodeData(body []byte) (any, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body), nil
	}
	if m, ok := v.(map[string]any); ok {
		if code, isNum := m["code"].(float64); isNum {
			if _, hasMsg := m["message"]; hasMsg {
				if int(code) == 0 || int(code) == 200 {
					return m["data"], nil
				}
				msg, _ := m["message"].(string)
				return nil, output.NewError(output.CodeQueryError,
					fmt.Sprintf("nacos error %d: %s", int(code), msg), "")
			}
		}
	}
	return v, nil
}

// api is the version-adapted Nacos API surface; the command layer never
// branches on the server major version. configGet returns the content plus
// the server-reported format ("" when the API generation does not carry
// it, e.g. 2.x).
type api interface {
	configGet(ctx context.Context, dataID, group, namespace string) (content, format string, err error)
	// configType resolves a config's server-recorded type, best-effort
	// ("" on failure); only needed where configGet cannot carry the
	// format (2.x).
	configType(ctx context.Context, dataID, group, namespace string) string
	configPublish(ctx context.Context, dataID, group, namespace, content, contentType string) error
	configDelete(ctx context.Context, dataID, group, namespace string) error
	configList(ctx context.Context, dataID, group, namespace string, pageNo, pageSize int) (*configPage, error)
	serviceList(ctx context.Context, namespace string, pageNo, pageSize int) (*servicePage, error)
	serviceDetail(ctx context.Context, service, group, namespace string) (any, error)
	instanceList(ctx context.Context, service, group, namespace string) ([]instanceInfo, error)
	namespaceList(ctx context.Context) ([]namespaceInfo, error)
}

func (c *client) api() api {
	if c.version == 3 {
		return apiV3{c}
	}
	return apiV2{c}
}

// configItem is one row of a config listing.
type configItem struct {
	DataID    string
	Group     string
	Namespace string
	Type      string
}

type configPage struct {
	Total int
	Items []configItem
}

// serviceItem is one row of a service listing.
type serviceItem struct {
	Name  string
	Group string
}

type servicePage struct {
	Total    int
	Services []serviceItem
}

// instanceInfo is one row of an instance listing.
type instanceInfo struct {
	IP      string
	Port    int
	Weight  float64
	Healthy bool
	Enabled bool
}

// namespaceInfo is one row of a namespace listing.
type namespaceInfo struct {
	Namespace   string
	ShowName    string
	Quota       int
	ConfigCount int
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

// namespaceOf resolves the effective namespace: the --namespace flag (or
// its --ns alias) overrides the connection's namespace (default public).
func namespaceOf(cmd *cobra.Command, conn Connection) string {
	if ns := cli.FlagString(cmd, "namespace"); ns != "" {
		return ns
	}
	if ns := cli.FlagString(cmd, "ns"); ns != "" {
		return ns
	}
	return conn.namespace()
}

// addNamespaceFlag registers --namespace with its --ns alias on a data
// command. cli.FlagString returns "" for unregistered flags, so
// namespaceOf tolerates commands that never call this.
func addNamespaceFlag(c *cobra.Command) {
	c.Flags().String("namespace", "", "namespace (overrides the connection's namespace, default public)")
	c.Flags().String("ns", "", "alias for --namespace")
}

// groupOf resolves the effective config/service group.
func groupOf(cmd *cobra.Command) string {
	if g := cli.FlagString(cmd, "group"); g != "" {
		return g
	}
	return "DEFAULT_GROUP"
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
