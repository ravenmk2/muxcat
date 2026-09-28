package rabbitmq

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newChannelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "channel",
		Short: "Inspect AMQP channels",
		Long: `Inspect AMQP channels: list them with their transactional
state and prefetch, or show one channel's full detail.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newChannelLsCmd(), newChannelShowCmd())
	return c
}

func newChannelLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List channels (GET /api/channels)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq channel ls
  muxcat rabbitmq channel ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/channels", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				ch := obj(item)
				mode := ""
				if boolOf(ch["transactional"]) {
					mode = "tx"
				}
				if boolOf(ch["confirm"]) {
					if mode != "" {
						mode += "+"
					}
					mode += "confirm"
				}
				rows = append(rows, []any{
					str(ch["name"]), str(ch["vhost"]), str(ch["user"]),
					str(ch["state"]), mode,
					numOf(ch["messages_unacknowledged"]), numOf(ch["prefetch_count"]),
					numOf(ch["consumer_count"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "vhost", "user", "state", "mode", "unacked", "prefetch", "consumers"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newChannelShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "show <name>",
		Short:   "Show one channel's full detail (GET /api/channels/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq channel show "127.0.0.1:50000 -> 127.0.0.1:5672 (1)"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/channels/"+esc(args[0]), nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    raw,
				JSONData: raw,
				Syntax:   "yaml",
			}, meta(name, start, false))
		},
	}
}
