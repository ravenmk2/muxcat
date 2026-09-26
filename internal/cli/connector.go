package cli

import (
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConnectorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connector",
		Short: "Manage connectors",
		Long: `Connectors are muxcat's built-in backend clients, each mounted as a
top-level command (e.g. redis, sqlite). This group inspects the
registry; use a connector's own command tree to work with a backend
(muxcat <connector> --help).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConnectorLsCmd())
	return c
}

func newConnectorLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered connectors",
		Args:  cobra.NoArgs,
		Example: `  muxcat connector ls
  muxcat connector ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := connector.Names()
			rows := make([][]any, 0, len(names))
			for _, n := range names {
				rows = append(rows, []any{n})
			}
			return RenderResult(cmd, &output.Result{
				Columns: []string{"name"},
				Rows:    rows,
			}, output.Meta{})
		},
	}
}
