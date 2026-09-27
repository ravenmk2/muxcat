package jenkins

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/output"
)

// Poll intervals of the --wait/--follow loops. They are package-level
// variables (not constants) so tests can shrink them.
var (
	queuePollInterval = 2 * time.Second
	buildPollInterval = 1 * time.Second
	logPollInterval   = 1 * time.Second
)

// defaultWaitTimeout bounds --wait/--follow polling when --wait-timeout is
// not passed. It is deliberately decoupled from the per-request --timeout.
const defaultWaitTimeout = 10 * time.Minute

// addWaitTimeoutFlag registers --wait-timeout on a polling command.
func addWaitTimeoutFlag(c *cobra.Command) {
	c.Flags().Duration("wait-timeout", defaultWaitTimeout,
		"maximum time to poll (--wait/--follow); independent of the per-request --timeout")
}

// waitTimeout reads and validates the --wait-timeout flag.
func waitTimeout(cmd *cobra.Command) (time.Duration, error) {
	d, _ := cmd.Flags().GetDuration("wait-timeout")
	if d <= 0 {
		return 0, output.NewError(output.CodeConfigInvalid,
			fmt.Sprintf("invalid --wait-timeout value: %v", d), "examples: 30s, 10m (must be > 0)")
	}
	return d, nil
}

// parseParams converts repeated --param k=v values into URL query values;
// Jenkins accepts build parameters as query arguments on
// buildWithParameters, so no body encoding is needed.
func parseParams(params []string) (url.Values, error) {
	v := url.Values{}
	for _, p := range params {
		k, val, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, output.NewError(output.CodeConfigInvalid,
				"invalid --param value: "+p, "expected k=v, e.g. --param ENV=uat")
		}
		v.Add(k, val)
	}
	return v, nil
}

// queueLocationRe extracts the item id from the Location header of a build
// trigger response (…/queue/item/{id}/).
var queueLocationRe = regexp.MustCompile(`/queue/item/(\d+)/?`)

// queueID parses the queue item id from the Location header of a 201 build
// trigger response. The header value itself is kept out of the error
// message; only the shape failure is reported.
func queueID(location string) (string, error) {
	m := queueLocationRe.FindStringSubmatch(location)
	if m == nil {
		return "", output.NewError(output.CodeQueryError,
			"build was queued but the response Location header has an unexpected form",
			"check the queue with muxcat jenkins queue ls")
	}
	return m[1], nil
}

// waitQueue polls a queue item until it is assigned a build number. On an
// idle server the item can leave the queue within milliseconds — before
// the first poll — so a 404 is the common case, not an edge: it is not an
// error, the build is then found by matching our queue id against the
// job's recent builds.
func waitQueue(ctx context.Context, cl *client, path, id string) (int64, error) {
	qid, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, output.NewError(output.CodeQueryError,
			"build was queued but the queue item id has an unexpected form", "")
	}
	for {
		resp, err := cl.do(ctx, "GET",
			"/queue/item/"+id+"/api/json?tree=executable[number,url],cancelled,why", nil)
		if werr := waitCtxErr(ctx); werr != nil {
			return 0, werr
		}
		switch {
		case err == nil:
			raw, derr := decodeBody(resp.body)
			if derr != nil {
				return 0, derr
			}
			m, _ := raw.(map[string]any)
			if boolOf(m["cancelled"]) {
				msg := "build was cancelled while queued"
				if why := str(m["why"]); why != "" {
					msg += ": " + why
				}
				return 0, output.NewError(output.CodeQueryError, msg, "")
			}
			if ex, ok := m["executable"].(map[string]any); ok {
				if n, ok := ex["number"].(float64); ok && n > 0 {
					return int64(n), nil
				}
			}
		case isNotFound(err):
			if n, found, ferr := buildByQueueID(ctx, cl, path, qid); ferr != nil {
				return 0, ferr
			} else if found {
				return n, nil
			}
		default:
			return 0, err
		}
		if err := sleepCtx(ctx, queuePollInterval); err != nil {
			return 0, waitCtxErr(ctx)
		}
	}
}

// buildByQueueID finds the build a queue item became by matching the queue
// id (a number in build records) against the job's recent builds.
func buildByQueueID(ctx context.Context, cl *client, path string, qid int64) (int64, bool, error) {
	resp, err := cl.do(ctx, "GET", path+"/api/json?tree=builds[number,queueId]{,20}", nil)
	if werr := waitCtxErr(ctx); werr != nil {
		return 0, false, werr
	}
	if err != nil {
		return 0, false, err
	}
	raw, err := decodeBody(resp.body)
	if err != nil {
		return 0, false, err
	}
	m, _ := raw.(map[string]any)
	builds, _ := m["builds"].([]any)
	for _, item := range builds {
		b, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if q, ok := b["queueId"].(float64); ok && int64(q) == qid {
			if n, ok := b["number"].(float64); ok && n > 0 {
				return int64(n), true, nil
			}
		}
	}
	return 0, false, nil
}

// waitBuild polls a build until it reaches a terminal state and returns
// the summary object.
func waitBuild(ctx context.Context, cl *client, path string, number int64) (map[string]any, error) {
	for {
		resp, err := cl.do(ctx, "GET",
			fmt.Sprintf("%s/%d/api/json?tree=number,displayName,result,building,timestamp,duration,url", path, number), nil)
		if werr := waitCtxErr(ctx); werr != nil {
			return nil, werr
		}
		if err != nil {
			return nil, err
		}
		raw, err := decodeBody(resp.body)
		if err != nil {
			return nil, err
		}
		m, _ := raw.(map[string]any)
		if !boolOf(m["building"]) && m["result"] != nil {
			return m, nil
		}
		if err := sleepCtx(ctx, buildPollInterval); err != nil {
			return nil, waitCtxErr(ctx)
		}
	}
}

// isNotFound reports whether err is the classified 404 form.
func isNotFound(err error) bool {
	e := output.ToError(err)
	return e.Code == output.CodeQueryError && strings.Contains(e.Message, "not found")
}

// sleepCtx sleeps d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitCtxErr maps a wait-loop context expiry to TIMEOUT pointing at
// --wait-timeout (the per-request --timeout hint would mislead here).
func waitCtxErr(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return output.NewError(output.CodeTimeout,
		"wait timed out", "increase --wait-timeout, and check the build state with muxcat jenkins queue ls / build ls")
}
