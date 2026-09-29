package elasticsearch

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

// masterKey is the master key resolver, a package-level variable so tests
// can replace it (keychain-less environments).
var masterKey = secret.MasterKey

// client is an authenticated Elasticsearch HTTP client. The password and
// apiKey fields hold decrypted credentials and live in memory only; they
// are never written to any output channel.
type client struct {
	baseURL  string
	username string
	password string
	apiKey   string
	hc       *http.Client
}

// newClient builds a client from an instance + connection, decrypting the
// enc:v1: credential blobs when present.
func newClient(cfg *Config, conn Connection, timeout time.Duration) (*client, error) {
	inst, err := cfg.instanceOf(conn)
	if err != nil {
		return nil, err
	}
	base, err := normalizeURL(inst.URL)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: timeout}
	if conn.InsecureSkipVerify {
		hc.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // explicit user opt-in on the connection
		}
	}
	c := &client{baseURL: base, username: conn.Username, hc: hc}
	if conn.Password != "" || conn.APIKey != "" {
		key, _, err := masterKey()
		if err != nil {
			return nil, err
		}
		if conn.Password != "" {
			plain, err := secret.Decrypt(key, conn.Password)
			if err != nil {
				return nil, err
			}
			c.password = string(plain)
		}
		if conn.APIKey != "" {
			plain, err := secret.Decrypt(key, conn.APIKey)
			if err != nil {
				return nil, err
			}
			c.apiKey = string(plain)
		}
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

// response is the raw carrier of an HTTP call.
type response struct {
	status  int
	headers map[string]string
	body    []byte
}

// do performs one HTTP call and classifies non-2xx statuses into structured
// errors. path is appended to the instance base URL verbatim.
func (c *client) do(ctx context.Context, method, path string, body []byte) (*response, error) {
	r, err := c.exchange(ctx, method, path, body, "")
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
// defaults Content-Type to application/json. compat ("7" or "8") switches
// Accept/Content-Type to the versioned media type
// application/vnd.elasticsearch+json;compatible-with=N.
func (c *client) exchange(ctx context.Context, method, path string, body []byte, compat string) (*response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, output.NewError(output.CodeConfigInvalid, "invalid request path: "+path, "")
	}
	if c.apiKey != "" {
		// Elasticsearch expects the base64(id:api_key) string verbatim.
		req.Header.Set("Authorization", "ApiKey "+c.apiKey)
	} else if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	mediaType := "application/json"
	if compat != "" {
		mediaType = "application/vnd.elasticsearch+json;compatible-with=" + compat
		req.Header.Set("Accept", mediaType)
	}
	if body != nil {
		req.Header.Set("Content-Type", mediaType)
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
// error text may contain the request URL but never credentials (auth
// travels in headers, and the base URL is validated userinfo-free).
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
		case strings.Contains(low, "certificate"):
			return output.NewError(output.CodeConnectFailed,
				"TLS certificate verification failed: "+unwrapURL(urlErr),
				"for a self-signed server (Elasticsearch 8/9 default), set insecureSkipVerify via conn add --insecure-skip-verify")
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

// classifyStatus maps a non-2xx status to a structured error, extracting
// the server's reason from Elasticsearch's error body
// ({"error":{"reason":...},"status":N} or {"error":"..."}).
func classifyStatus(status int, body []byte) *output.Error {
	msg := errorReason(body)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 512))
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return output.NewError(output.CodeAuthFailed, msg,
			"check the credentials of the connection")
	}
	return output.NewError(output.CodeQueryError, msg, "")
}

// errorReason extracts the reason from an Elasticsearch error body; the
// "error" field is an object on modern versions and a plain string on old
// ones. Best-effort: "" when the shape does not match.
func errorReason(body []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || len(e.Error) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Error, &s); err == nil {
		return s
	}
	var obj struct {
		Reason string `json:"reason"`
		Type   string `json:"type"`
	}
	if err := json.Unmarshal(e.Error, &obj); err != nil {
		return ""
	}
	if obj.Reason == "" {
		return obj.Type
	}
	return obj.Reason
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
