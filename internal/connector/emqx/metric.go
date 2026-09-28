package emqx

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newMetricCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "metric",
		Short: "Inspect metrics and stats counters",
		Long: `Inspect metrics and stats counters. metric ls merges GET
/api/v5/metrics and GET /api/v5/stats (both node -> {name: value}) into
one table of {node, kind(metric|stat), name, value}; --node and --match
filter client-side.

Quickstart:
  1. muxcat emqx metric ls --match messages.received
  2. muxcat emqx metric ls --node emqx@127.0.0.1`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newMetricLsCmd())
	return c
}

func newMetricLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List metrics and stats counters (merged per node)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx metric ls
  muxcat emqx metric ls --match messages --limit 50
  muxcat emqx metric ls --node emqx@127.0.0.1 --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			nodeFilter := cli.FlagString(cmd, "node")
			match := cli.FlagString(cmd, "match")

			rows := [][]any{}
			// /metrics and /stats share the shape [{node, <name>: value, ...}]:
			// one flat object per node, every key except "node" is a counter.
			for _, src := range []struct {
				path string
				kind string
			}{
				{"/api/v5/metrics", "metric"},
				{"/api/v5/stats", "stat"},
			} {
				r, err := cl.send(cmd.Context(), http.MethodGet, src.path, nil, nil, false)
				if err != nil {
					return err
				}
				var list []map[string]any
				if err := decodeJSON(r.body, &list); err != nil {
					return err
				}
				for _, entry := range list {
					node := strOf(entry, "node")
					if nodeFilter != "" && node != nodeFilter {
						continue
					}
					names := make([]string, 0, len(entry))
					for n := range entry {
						if n == "node" {
							continue
						}
						names = append(names, n)
					}
					sort.Strings(names)
					for _, n := range names {
						if match != "" && !strings.Contains(n, match) {
							continue
						}
						rows = append(rows, []any{node, src.kind, n, entry[n]})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"node", "kind", "name", "value"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("node", "", "filter by node name (client-side)")
	c.Flags().String("match", "", "filter counters whose name contains this substring (client-side)")
	return c
}
