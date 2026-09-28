package emqx

import (
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newNodeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "node",
		Short: "Inspect cluster nodes",
		Long: `Inspect EMQX cluster nodes. node ls lists each node with its
version, uptime and connection counters; node show prints one node's
full detail (default: the first node).

Quickstart:
  1. muxcat emqx node ls
  2. muxcat emqx node show emqx@127.0.0.1`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newNodeLsCmd(),
		newNodeShowCmd(),
	)
	return c
}

func newNodeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List cluster nodes",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx node ls
  muxcat emqx node ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/nodes", nil, nil, false)
			if err != nil {
				return err
			}
			var nodes []map[string]any
			if err := decodeJSON(r.body, &nodes); err != nil {
				return err
			}
			rows := make([][]any, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []any{
					strOf(n, "node"),
					strOf(n, "version"),
					strOf(n, "edition"),
					fmtUptime(intOf(n, "uptime")),
					intOf(n, "connections"),
					intOf(n, "live_connections"),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"node", "version", "edition", "uptime", "connections", "live_connections"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
}

func newNodeShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [node]",
		Short: "Show a node's full detail (default: the first node)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  muxcat emqx node show emqx@127.0.0.1
  muxcat emqx node show --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			node := ""
			if len(args) > 0 {
				node = args[0]
			} else {
				// Default to the first node of the cluster.
				r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/nodes", nil, nil, false)
				if err != nil {
					return err
				}
				var nodes []map[string]any
				if err := decodeJSON(r.body, &nodes); err != nil {
					return err
				}
				if len(nodes) == 0 {
					return output.NewError(output.CodeQueryError, "GET /api/v5/nodes returned no nodes", "")
				}
				node = strOf(nodes[0], "node")
			}
			r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/nodes/"+url.PathEscape(node), nil, nil, false)
			if err != nil {
				return err
			}
			var v map[string]any
			if err := decodeJSON(r.body, &v); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{Value: v, Syntax: "yaml"}, meta(name, start, false))
		},
	}
}
