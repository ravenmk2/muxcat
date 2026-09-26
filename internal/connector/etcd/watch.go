package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spf13/cobra"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newWatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch <key>",
		Short: "Watch a key or prefix and collect a bounded batch of events",
		Long: `Watch a key (or a prefix with --prefix) and collect events until
--max-events have been seen or --timeout elapses, then print the
collected batch at once. --rev starts watching at a historical
revision. This is a bounded collector, not a follower: the command
always terminates on its own.`,
		Args: cli.ExactArgs(1, "<key>", "key"),
		Example: `  muxcat etcd watch mykey
  muxcat etcd watch /services/ --prefix --max-events 20 --timeout 30s
  muxcat etcd watch mykey --rev 100 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			maxEvents, _ := cmd.Flags().GetInt("max-events")
			if maxEvents <= 0 {
				return output.NewError(output.CodeMissingArgument,
					"--max-events must be > 0", "")
			}
			rev, _ := cmd.Flags().GetInt64("rev")
			if rev < 0 {
				return output.NewError(output.CodeMissingArgument,
					"--rev must be >= 0", "")
			}
			cfg, name, conn, err := resolveTarget(cmd)
			if err != nil {
				return err
			}
			timeout, err := queryTimeout(conn, mustWatchTimeout(cmd))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			client, err := openClient(ctx, cfg, conn, timeout)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			var opts []clientv3.OpOption
			if cli.FlagBool(cmd, "prefix") {
				opts = append(opts, clientv3.WithPrefix())
			}
			if rev > 0 {
				opts = append(opts, clientv3.WithRev(rev))
			}
			ch := client.Watch(ctx, args[0], opts...)
			events := make([]map[string]any, 0, maxEvents)
		collect:
			for len(events) < maxEvents {
				select {
				case resp, ok := <-ch:
					if !ok {
						break collect
					}
					if err := resp.Err(); err != nil {
						return classifyErr(err, "watch failed")
					}
					for _, ev := range resp.Events {
						events = append(events, watchEvent(ev))
						if len(events) >= maxEvents {
							break collect
						}
					}
				case <-ctx.Done():
					break collect
				}
			}
			items := make([]any, len(events))
			for i, e := range events {
				items[i] = e
			}
			res := &output.Result{JSONData: items}
			if b, err := json.MarshalIndent(events, "", "  "); err == nil {
				res.Value = string(b)
				res.Syntax = "json"
			}
			return cli.RenderResult(cmd, res, meta(cfg, conn, name, start, false))
		},
	}
	c.Flags().Bool("prefix", false, "watch every key under the prefix")
	c.Flags().Int64("rev", 0, "start watching at this revision (0 = current)")
	c.Flags().Int("max-events", 10, "stop collecting after this many events")
	c.Flags().Duration("timeout", 10*time.Second, "stop collecting after this duration (shadows the global --timeout)")
	return c
}

// mustWatchTimeout reads the watch-local --timeout flag (it shadows the
// global one on this command).
func mustWatchTimeout(cmd *cobra.Command) time.Duration {
	d, _ := cmd.Flags().GetDuration("timeout")
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}

// watchEvent converts a clientv3 event to the envelope shape; DELETE
// events carry no value.
func watchEvent(ev *clientv3.Event) map[string]any {
	e := map[string]any{
		"type":         ev.Type.String(),
		"key":          string(ev.Kv.Key),
		"mod_revision": ev.Kv.ModRevision,
	}
	if ev.Type != mvccpb.DELETE {
		e["value"] = string(ev.Kv.Value)
	}
	return e
}
