package emqx

import (
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newListenerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "listener",
		Short: "Inspect protocol listeners",
		Long: `Inspect EMQX protocol listeners (MQTT/WebSocket/SSL/QUIC
ports). listener ls lists each listener with its bind address, running
state and connection counters.

Quickstart:
  1. muxcat emqx listener ls`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newListenerLsCmd())
	return c
}

func newListenerLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List listeners",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx listener ls
  muxcat emqx listener ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/listeners", nil, nil, false)
			if err != nil {
				return err
			}
			var listeners []map[string]any
			if err := decodeJSON(r.body, &listeners); err != nil {
				return err
			}
			rows := make([][]any, 0, len(listeners))
			for _, l := range listeners {
				// Counters live in the nested status object; max_connections
				// is the string "infinity" when unbounded.
				st, _ := l["status"].(map[string]any)
				rows = append(rows, []any{
					strOf(l, "id"),
					strOf(l, "type"),
					strOf(l, "name"),
					strOf(l, "bind"),
					st["running"],
					st["current_connections"],
					st["max_connections"],
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"id", "type", "name", "bind", "running", "current_connections", "max_connections"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
}
