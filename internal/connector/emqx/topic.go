package emqx

import (
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newTopicCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "topic",
		Short: "Inspect the topic (route) table",
		Long: `Inspect the topic table. EMQX 5.x has no /routes endpoint; the
route table is served as /api/v5/topics, one row per (topic, node)
pair, paginated. topic ls optionally filters by exact topic.

Quickstart:
  1. muxcat emqx topic ls
  2. muxcat emqx topic ls --topic "sensors/temp"`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newTopicLsCmd())
	return c
}

func newTopicLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List topics (the route table; paginated)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx topic ls
  muxcat emqx topic ls --topic "sensors/temp"
  muxcat emqx topic ls --limit 100 --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			params := url.Values{}
			if v := cli.FlagString(cmd, "topic"); v != "" {
				params.Set("topic", v)
			}
			items, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/topics", params, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, []any{
					strOf(it, "topic"),
					strOf(it, "node"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"topic", "node"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("topic", "", "filter by exact topic")
	return c
}
