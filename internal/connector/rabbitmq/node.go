package rabbitmq

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newNodeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "node",
		Short: "Inspect RabbitMQ cluster nodes",
		Long: `Inspect RabbitMQ cluster nodes: list members with memory,
disk and uptime figures, or show one node's full detail document.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newNodeLsCmd(), newNodeShowCmd())
	return c
}

func newNodeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List cluster nodes (GET /api/nodes)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq node ls
  muxcat rabbitmq node ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/nodes", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				n := obj(item)
				rows = append(rows, []any{
					str(n["name"]), boolOf(n["running"]),
					str(n["type"]),
					megaBytes(n["mem_used"]), megaBytes(n["disk_free"]),
					numOf(n["fd_used"]), numOf(n["sockets_used"]),
					uptimeStr(n["uptime"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "running", "type", "mem_used", "disk_free", "fd_used", "sockets_used", "uptime"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newNodeShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one node's full detail (GET /api/nodes/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq node show rabbit@host1
  muxcat rabbitmq node show rabbit@host1 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/nodes/"+esc(args[0]), nil)
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

// megaBytes renders a byte count as MiB, e.g. "512MiB"; "" when absent.
func megaBytes(v any) string {
	f, ok := v.(float64)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%dMiB", int64(f)>>20)
}

// uptimeStr renders milliseconds of uptime as a compact duration.
func uptimeStr(v any) string {
	f, ok := v.(float64)
	if !ok {
		return ""
	}
	d := time.Duration(f) * time.Millisecond
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
