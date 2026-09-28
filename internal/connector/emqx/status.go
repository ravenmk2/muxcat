package emqx

import (
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Cluster overview: node status, version and current counters",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx status
  muxcat emqx status -c prod --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// Aggregated overview: /nodes (version/edition/uptime/running
			// state) and /monitor_current (live counters). /api/v5/status
			// is plain text, so the running state comes from each node's
			// node_status field instead.
			var nodes []map[string]any
			r, err := cl.send(ctx, http.MethodGet, "/api/v5/nodes", nil, nil, false)
			if err != nil {
				return err
			}
			if err := decodeJSON(r.body, &nodes); err != nil {
				return err
			}
			var monitor map[string]any
			r, err = cl.send(ctx, http.MethodGet, "/api/v5/monitor_current", nil, nil, false)
			if err != nil {
				return err
			}
			if err := decodeJSON(r.body, &monitor); err != nil {
				return err
			}

			nodeRows := make([]any, 0, len(nodes))
			version, edition := "", ""
			for _, n := range nodes {
				if version == "" {
					version = strOf(n, "version")
				}
				if edition == "" {
					edition = strOf(n, "edition")
				}
				nodeRows = append(nodeRows, map[string]any{
					"node":             strOf(n, "node"),
					"status":           strOf(n, "node_status"),
					"uptime":           fmtUptime(intOf(n, "uptime")),
					"connections":      intOf(n, "connections"),
					"live_connections": intOf(n, "live_connections"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{Value: map[string]any{
				"version": version,
				"edition": edition,
				"nodes":   nodeRows,
				"cluster": monitor,
			}, Syntax: "yaml"}, meta(name, start, false))
		},
	}
}
