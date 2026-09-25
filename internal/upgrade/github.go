package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"time"

	"github.com/ravenmk2/muxcat/internal/output"
)

const (
	defaultAPIBase      = "https://api.github.com"
	defaultDownloadBase = "https://github.com"
	repoOwner           = "ravenmk2"
	repoName            = "muxcat"
)

// Client talks to the GitHub releases API and download endpoints. APIBase
// and DownloadBase are fields (not constants) so tests can point them at an
// httptest server.
type Client struct {
	APIBase      string
	DownloadBase string
	HTTP         *http.Client
	// Token is an optional GitHub token (GITHUB_TOKEN env) raising the
	// anonymous API rate limit.
	Token string
	// IdleTimeout aborts a single download attempt that receives no data
	// for this long (stall detection), making the attempt retryable. Slow
	// but steadily progressing downloads are unaffected. <= 0 disables it.
	IdleTimeout time.Duration
	// Backoff returns the wait before retry number n (1-based); nil uses
	// defaultBackoff. Tests inject an instant backoff.
	Backoff func(retry int) time.Duration
}

// NewClient builds a Client with production endpoints, honoring the
// GITHUB_TOKEN environment variable.
func NewClient() *Client {
	return &Client{
		APIBase:      defaultAPIBase,
		DownloadBase: defaultDownloadBase,
		HTTP:         http.DefaultClient,
		Token:        os.Getenv("GITHUB_TOKEN"),
		IdleTimeout:  30 * time.Second,
	}
}

// Release is a GitHub release. Version is Tag without the leading "v".
type Release struct {
	Tag     string
	Version string
}

// LatestRelease resolves the newest stable release (the endpoint never
// returns prereleases).
func (c *Client) LatestRelease(ctx context.Context) (*Release, error) {
	var payload struct {
		TagName string `json:"tag_name"`
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.APIBase, repoOwner, repoName)
	if err := c.getJSON(ctx, url, &payload); err != nil {
		return nil, err
	}
	return &Release{Tag: payload.TagName, Version: normalizeVersion(payload.TagName)}, nil
}

// ReleaseByTag resolves the release for an explicit version (with or
// without the leading "v").
func (c *Client) ReleaseByTag(ctx context.Context, version string) (*Release, error) {
	v := normalizeVersion(version)
	var payload struct {
		TagName string `json:"tag_name"`
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/v%s", c.APIBase, repoOwner, repoName, v)
	if err := c.getJSON(ctx, url, &payload); err != nil {
		return nil, err
	}
	return &Release{Tag: payload.TagName, Version: normalizeVersion(payload.TagName)}, nil
}

func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return output.NewError(output.CodeTimeout, "timed out querying the GitHub API", "increase --timeout or check your network")
		}
		return output.NewError(output.CodeConnectFailed, "failed to reach the GitHub API: "+err.Error(), "check your network or proxy settings")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return output.NewError(output.CodeUpgradeFailed, "release not found ("+url+")", "check the version with muxcat upgrade --check")
	}
	if resp.StatusCode != http.StatusOK {
		return output.NewError(output.CodeConnectFailed, "GitHub API returned "+resp.Status, "retry later or set GITHUB_TOKEN to raise the rate limit")
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return output.NewError(output.CodeConnectFailed, "malformed GitHub API response: "+err.Error(), "")
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", repoName)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// DownloadAsset downloads a release asset into dest with up to attempts
// tries. Network errors, stalls (IdleTimeout without data), 5xx and 429
// are retried with exponential backoff; other 4xx fail immediately.
// onProgress (may be nil) reports bytes received and the total (0 when
// unknown).
func (c *Client) DownloadAsset(ctx context.Context, rel *Release, name string, dest *os.File, attempts int, onProgress func(received, total int64)) error {
	if attempts < 1 {
		attempts = 1
	}
	url := fmt.Sprintf("%s/%s/%s/releases/download/%s/%s", c.DownloadBase, repoOwner, repoName, rel.Tag, name)
	var lastErr error
	made := 0
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			if err := c.waitRetry(ctx, attempt-1); err != nil {
				return err
			}
		}
		made++
		retryable, err := c.fetch(ctx, url, dest, onProgress)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt >= attempts {
			break
		}
	}
	return output.NewError(output.CodeConnectFailed,
		fmt.Sprintf("download failed after %d attempt(s): %s", made, lastErr),
		"check your network or proxy settings, or raise --attempts")
}

func (c *Client) waitRetry(ctx context.Context, retry int) error {
	d := defaultBackoff(retry)
	if c.Backoff != nil {
		d = c.Backoff(retry)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return output.NewError(output.CodeTimeout, "timed out while downloading", "increase --timeout")
	case <-timer.C:
		return nil
	}
}

func (c *Client) fetch(ctx context.Context, url string, dest *os.File, onProgress func(received, total int64)) (retryable bool, err error) {
	// The attempt context is canceled either by the parent (overall
	// command timeout — not retryable) or by the stall watchdog
	// (IdleTimeout without data — retryable).
	attemptCtx := ctx
	cancel := context.CancelFunc(func() {})
	if c.IdleTimeout > 0 {
		attemptCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	c.setHeaders(req)

	var watchdog *time.Timer
	if c.IdleTimeout > 0 {
		watchdog = time.AfterFunc(c.IdleTimeout, cancel)
		defer watchdog.Stop()
	}
	resetWatchdog := func() {
		if watchdog != nil {
			watchdog.Reset(c.IdleTimeout)
		}
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.classifyFetchError(ctx, attemptCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return true, fmt.Errorf("unexpected status %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %s", resp.Status)
	}
	resetWatchdog() // headers received; body activity keeps it alive from here
	if err := dest.Truncate(0); err != nil {
		return false, err
	}
	if _, err := dest.Seek(0, 0); err != nil {
		return false, err
	}
	body := io.Reader(resp.Body)
	if onProgress != nil || watchdog != nil {
		body = &readReporter{r: resp.Body, total: resp.ContentLength, fn: func(received, total int64) {
			resetWatchdog()
			if onProgress != nil {
				onProgress(received, total)
			}
		}}
	}
	if _, err := io.Copy(dest, body); err != nil {
		return c.classifyFetchError(ctx, attemptCtx, err)
	}
	return false, nil
}

// classifyFetchError maps a failed attempt: parent-context cancellation is
// the overall command timeout (not retryable); attempt-context cancellation
// is the stall watchdog (retryable); anything else is a plain network error
// (retryable).
func (c *Client) classifyFetchError(ctx, attemptCtx context.Context, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, output.NewError(output.CodeTimeout, "timed out while downloading", "increase --timeout")
	}
	if attemptCtx.Err() != nil {
		return true, fmt.Errorf("download stalled (no data for %s)", c.IdleTimeout)
	}
	return true, err
}

// readReporter reports read progress to fn.
type readReporter struct {
	r     io.Reader
	total int64
	n     int64
	fn    func(received, total int64)
}

func (p *readReporter) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	p.fn(p.n, p.total)
	return n, err
}

// defaultBackoff waits 500ms * 2^(retry-1) capped at 5s, with full jitter.
func defaultBackoff(retry int) time.Duration {
	d := 500 * time.Millisecond
	for i := 1; i < retry && d < 5*time.Second; i++ {
		d *= 2
	}
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}
