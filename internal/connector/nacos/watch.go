package nacos

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConfigWatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch <dataId>",
		Short: "Watch a config and stream its content on every change",
		Long: `Watch a config and stream its content: the current content
first, then the new content on every change, until interrupted
(Ctrl+C exits cleanly).

On 2.x servers this uses the HTTP long-polling listener (the server
holds the subscription for 30s and answers early on change; --interval
is ignored). 3.x removed that endpoint, so watch degrades to polling
the client read API every --interval (default 5s) and comparing md5.
The login token is refreshed ahead of its ttl for long sessions.
Text streaming only; --json is not supported.`,
		Args: cli.ExactArgs(1, "<dataId>", "dataId"),
		Example: `  muxcat nacos config watch app.yaml
  muxcat nacos config watch app.yaml -g BIZ_GROUP --interval 2s`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cli.FlagBool(cmd, "json") || cli.FlagString(cmd, "output") == "json" {
				return output.NewError(output.CodeUnsupportedOperation,
					"config watch streams text and does not support JSON output", "")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			_, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
			if err != nil {
				return err
			}
			// No client-level deadline: a long-poll subscription holds for
			// 30s by design. The transport's dial timeout still bounds
			// connection setup.
			cl, err := newClient(cfg, conn, 0)
			if err != nil {
				return err
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			dataID := args[0]
			group := groupOf(cmd)
			namespace := namespaceOf(cmd, conn)

			// Initial state; a missing config is watched from empty.
			content, err := cl.api().configGet(cmd.Context(), dataID, group, namespace)
			if err != nil {
				if !isNotFound(err) {
					return err
				}
				content = ""
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "# config %s does not exist yet; watching for creation\n", dataID)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), content)
			sum := md5Hex(content)

			for {
				changed, err := waitChange(cmd.Context(), cl, dataID, group, namespace, sum, cli.FlagString(cmd, "interval"))
				if err != nil {
					if cmd.Context().Err() != nil {
						return nil
					}
					return err
				}
				if !changed {
					continue
				}
				content, err = cl.api().configGet(cmd.Context(), dataID, group, namespace)
				if err != nil {
					if isNotFound(err) {
						content = ""
					} else {
						return err
					}
				}
				newSum := md5Hex(content)
				if newSum == sum {
					continue
				}
				sum = newSum
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "--- changed %s ---\n%s\n", time.Now().Format(time.RFC3339), content)
			}
		},
	}
	addAddressFlags(c)
	c.Flags().String("interval", "5s", "poll interval on 3.x servers (ignored on 2.x, which long-polls)")
	return c
}

// waitChange blocks until the config's content differs from sum. On 2.x
// it issues one long-poll subscription; on 3.x one md5 comparison poll
// per --interval.
func waitChange(ctx context.Context, cl *client, dataID, group, namespace, sum, interval string) (bool, error) {
	if cl.version == 3 {
		d, err := time.ParseDuration(interval)
		if err != nil || d <= 0 {
			d = 5 * time.Second
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false, nil
		case <-timer.C:
		}
		content, err := cl.api().configGet(ctx, dataID, group, namespace)
		if err != nil {
			if isNotFound(err) {
				return sum != md5Hex(""), nil
			}
			return false, err
		}
		return md5Hex(content) != sum, nil
	}
	return longPoll(ctx, cl, dataID, group, namespace, sum)
}

// longPoll issues one 2.x listener subscription. The server answers with
// the changed config's coordinates (dataId\x02group\x02tenant\x01) on
// change, or an empty body when the hold time elapses.
func longPoll(ctx context.Context, cl *client, dataID, group, namespace, sum string) (bool, error) {
	listenKey := dataID + "\x02" + group + "\x02" + sum + "\x02" + ns2(namespace) + "\x01"
	body := []byte("Listening-Configs=" + url.QueryEscape(listenKey))
	headers := map[string]string{
		"Content-Type":         "application/x-www-form-urlencoded",
		"Long-Pulling-Timeout": "30000",
	}
	r, err := cl.send(ctx, http.MethodPost, "/nacos/v1/cs/configs/listener", nil, body, headers)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(r.body)) != "", nil
}

func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// isNotFound reports whether err is the classified 404.
func isNotFound(err error) bool {
	e := output.ToError(err)
	return e.Code == output.CodeQueryError && strings.Contains(e.Message, "not found")
}
