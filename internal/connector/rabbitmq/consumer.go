package rabbitmq

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConsumerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "consumer",
		Short: "Inspect consumers",
		Long: `Inspect consumers: the subscriptions currently attached to
queues across the broker (or one vhost).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConsumerLsCmd())
	return c
}

func newConsumerLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List consumers (GET /api/consumers)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq consumer ls
  muxcat rabbitmq consumer ls --vhost /`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := "/api/consumers"
			if cmd.Flags().Changed("vhost") {
				path += "/" + esc(vhostFlag(cmd))
			}
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				cn := obj(item)
				rows = append(rows, []any{
					str(obj(cn["queue"])["name"]), str(cn["vhost"]),
					str(obj(cn["channel_details"])["name"]),
					str(cn["consumer_tag"]), boolOf(cn["ack_required"]),
					numOf(cn["prefetch_count"]), boolOf(cn["active"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"queue", "vhost", "channel", "consumer_tag", "ack_required", "prefetch", "active"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	return c
}
