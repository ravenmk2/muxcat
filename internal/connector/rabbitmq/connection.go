package rabbitmq

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConnectionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connection",
		Short: "Inspect AMQP client connections of the broker",
		Long: `Inspect AMQP client connections of the broker: list them,
show one connection's full detail, or force-close one. (These are the
broker's inbound AMQP connections; for muxcat's own stored
connections, see the conn group.)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConnectionLsCmd(), newConnectionShowCmd(), newConnectionCloseCmd())
	return c
}

func newConnectionLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List AMQP client connections (GET /api/connections)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq connection ls
  muxcat rabbitmq connection ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/connections", nil)
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
					str(cn["name"]), str(cn["user"]), str(cn["vhost"]),
					str(cn["state"]), numOf(cn["channels"]),
					peerAddr(cn), str(cn["protocol"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "user", "vhost", "state", "channels", "peer", "protocol"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newConnectionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one AMQP connection's full detail (GET /api/connections/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq connection show "127.0.0.1:50000 -> 127.0.0.1:5672"
  muxcat rabbitmq connection show "127.0.0.1:50000 -> 127.0.0.1:5672" --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			// Connection names contain spaces and " -> "; they must be
			// path-escaped before joining the API path.
			resp, err := cl.do(cmd.Context(), "GET", "/api/connections/"+esc(args[0]), nil)
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

func newConnectionCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "close <name>",
		Short:   "Force-close an AMQP connection (DELETE /api/connections/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq connection close "127.0.0.1:50000 -> 127.0.0.1:5672"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "connection close"); err != nil {
				return err
			}
			if _, err := cl.do(cmd.Context(), "DELETE", "/api/connections/"+esc(args[0]), nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"connection": args[0], "closed": true},
				Message: "closed connection " + args[0],
			}, meta(name, start, false))
		},
	}
}

// peerAddr renders the peer host:port of a connection.
func peerAddr(cn map[string]any) string {
	host, port := str(cn["peer_host"]), numOf(cn["peer_port"])
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", host, port)
}
