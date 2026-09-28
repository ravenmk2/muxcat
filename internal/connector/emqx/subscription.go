package emqx

import (
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newSubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sub",
		Short: "Inspect subscriptions",
		Long: `Inspect subscriptions. sub ls lists subscriptions (paginated),
optionally filtered by clientid or topic.

Quickstart:
  1. muxcat emqx sub ls
  2. muxcat emqx sub ls --clientid my-client-id`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newSubLsCmd())
	return c
}

func newSubLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List subscriptions (paginated)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx sub ls
  muxcat emqx sub ls --topic "sensors/#" --limit 50
  muxcat emqx sub ls --clientid my-client-id --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			params := url.Values{}
			if v := cli.FlagString(cmd, "clientid"); v != "" {
				params.Set("clientid", v)
			}
			if v := cli.FlagString(cmd, "topic"); v != "" {
				params.Set("topic", v)
			}
			items, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/subscriptions", params, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, []any{
					strOf(it, "clientid"),
					strOf(it, "topic"),
					intOf(it, "qos"),
					strOf(it, "node"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"clientid", "topic", "qos", "node"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("clientid", "", "filter by exact clientid")
	c.Flags().String("topic", "", "filter by exact topic (shared subscription: share/<group>/<topic>)")
	return c
}
