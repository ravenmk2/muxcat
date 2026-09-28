package emqx

import (
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newAlarmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "alarm",
		Short: "Inspect alarms",
		Long: `Inspect EMQX alarms. alarm ls lists currently activated alarms
by default; --history lists deactivated (historical) ones.

Quickstart:
  1. muxcat emqx alarm ls
  2. muxcat emqx alarm ls --history`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newAlarmLsCmd())
	return c
}

func newAlarmLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List alarms (activated by default; --history for deactivated)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx alarm ls
  muxcat emqx alarm ls --history
  muxcat emqx alarm ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			activated := "true"
			if cli.FlagBool(cmd, "history") {
				activated = "false"
			}
			params := url.Values{"activated": {activated}}
			alarms, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/alarms", params, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(alarms))
			for _, a := range alarms {
				rows = append(rows, []any{
					strOf(a, "name"),
					strOf(a, "node"),
					strOf(a, "message"),
					strOf(a, "activate_at"),
					strOf(a, "deactivate_at"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "node", "message", "activate_at", "deactivate_at"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().Bool("history", false, "list deactivated (historical) alarms instead of activated ones")
	return c
}
