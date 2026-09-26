// Package jenkins implements muxcat's Jenkins connector: a plain net/http
// client for Jenkins' REST API (HTTP basic auth with username + API token).
// Read-only inspection is covered: job/folder browsing, build history and
// console logs, queue and node status, plus a raw request passthrough.
// Write operations (trigger/stop builds, enable/disable jobs) are out of
// scope for this version.
package jenkins

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
	connector.Register("jenkins", New)
}

// New builds the jenkins connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:     "jenkins",
		Aliases: []string{"jk"},
		Short:   "Jenkins connector",
		Long: `Jenkins connector over the REST API (HTTP basic auth with an API
token), a plain net/http client with no external driver.

Quickstart:
  1. muxcat jenkins conn add local --url http://127.0.0.1:8080 --username admin --set-default
  2. muxcat jenkins job ls
  3. muxcat jenkins build log my-job last

A connection carries credentials (username + API token, encrypted at
rest, never echoed) and policies: readonly allows GET/HEAD requests
only. The command name is jenkins (alias jk). Jobs inside folders are
addressed by full name (folder/sub/job). Build triggering and other
write operations are out of scope — use request to pass any endpoint
through.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newJobCmd(),
		newBuildCmd(),
		newQueueCmd(),
		newNodeCmd(),
		newRequestCmd(),
	)
	return c
}

// meta builds the envelope meta for jenkins commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "jenkins",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}

// client is an authenticated Jenkins HTTP client. The token field holds
// the decrypted credential and lives in memory only; it is never written
// to any output channel.
type client struct {
	baseURL  string
	username string
	token    string
	hc       *http.Client

	// CSRF crumb state: Jenkins requires a crumb header on unsafe methods
	// when the default CSRF protection is enabled. crumbTried remembers
	// that /crumbIssuer was queried (servers without the crumb issuer
	// answer 404, meaning no crumb is needed).
	crumbField string
	crumb      string
	crumbTried bool
}

// newClient builds a client from an instance + connection, decrypting the
// enc:v1: token blob when present.
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
	if conn.Token != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		plain, err := secret.Decrypt(key, conn.Token)
		if err != nil {
			return nil, err
		}
		c.token = string(plain)
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
// structured errors. path is appended to the instance base URL verbatim.
func (c *client) do(ctx context.Context, method, path string, body []byte) (*response, error) {
	r, err := c.send(ctx, method, path, body)
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
// passthrough command.
func (c *client) exchange(ctx context.Context, method, path string, body []byte) (*response, error) {
	return c.send(ctx, method, path, body)
}

// send performs one HTTP call, attaching a CSRF crumb to unsafe methods.
// When Jenkins rejects the crumb (403 "No valid crumb", e.g. after a
// server restart), the crumb is refreshed once and the call retried.
func (c *client) send(ctx context.Context, method, path string, body []byte) (*response, error) {
	safe := method == "GET" || method == "HEAD"
	if !safe {
		c.ensureCrumb(ctx)
	}
	r, err := c.roundTrip(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if !safe && r.status == http.StatusForbidden && strings.Contains(string(r.body), "No valid crumb") {
		c.crumbField, c.crumb, c.crumbTried = "", "", false
		c.ensureCrumb(ctx)
		r, err = c.roundTrip(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// ensureCrumb fetches and caches the CSRF crumb. A 404 means the crumb
// issuer is disabled and unsafe methods need no crumb. Any other failure
// is ignored: the real request surfaces its own error, which is more
// useful than a crumb-fetch error.
func (c *client) ensureCrumb(ctx context.Context) {
	if c.crumbTried {
		return
	}
	c.crumbTried = true
	r, err := c.roundTrip(ctx, "GET", "/crumbIssuer/api/json", nil)
	if err != nil || r.status != http.StatusOK {
		return
	}
	var v struct {
		CrumbRequestField string `json:"crumbRequestField"`
		Crumb             string `json:"crumb"`
	}
	if json.Unmarshal(r.body, &v) == nil && v.CrumbRequestField != "" {
		c.crumbField, c.crumb = v.CrumbRequestField, v.Crumb
	}
}

// roundTrip performs the raw HTTP call. A nil body means no request body;
// a non-nil body defaults Content-Type to application/json.
func (c *client) roundTrip(ctx context.Context, method, path string, body []byte) (*response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "invalid request path: "+path, "")
	}
	if c.username != "" || c.token != "" {
		req.SetBasicAuth(c.username, c.token)
	}
	if c.crumbField != "" {
		req.Header.Set(c.crumbField, c.crumb)
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

// classifyStatus maps a non-2xx status to a structured error. Jenkins
// answers errors with HTML pages, so the body is truncated to keep the
// message readable.
func classifyStatus(status int, body []byte) *output.Error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return output.NewError(output.CodeAuthFailed,
			fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 512)),
			"check the username/token of the connection (Jenkins expects an API token, not the web password)")
	case http.StatusNotFound:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("not found (HTTP %d)", status),
			"check the job full name (folder/sub/job format) and build number or alias")
	default:
		return output.NewError(output.CodeQueryError,
			fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 512)), "")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// jobPath converts a job full name (folder/sub/job) to Jenkins' URL form
// (job/folder/job/sub/job/job), path-escaping each segment.
func jobPath(fullName string) string {
	segs := strings.Split(strings.Trim(fullName, "/"), "/")
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteString("/job/")
		sb.WriteString(url.PathEscape(s))
	}
	return sb.String()
}

// buildRef resolves a build reference: an alias (last, lastSuccessful,
// lastFailed, lastCompleted) or a build number.
func buildRef(s string) (string, error) {
	switch s {
	case "last":
		return "lastBuild", nil
	case "lastSuccessful", "lastFailed", "lastCompleted":
		return s + "Build", nil
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return strconv.Itoa(n), nil
	}
	return "", output.NewError(output.CodeConfigInvalid,
		"invalid build reference: "+s,
		"use a build number or one of: last, lastSuccessful, lastFailed, lastCompleted")
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

// decodeBody parses a response body as JSON into a generic value.
func decodeBody(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, output.NewError(output.CodeQueryError,
			"response is not valid JSON: "+err.Error(), "")
	}
	return v, nil
}

// applyLimit truncates rows to the global --limit (0 disables truncation).
func applyLimit(rows [][]any, limit int) ([][]any, bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
