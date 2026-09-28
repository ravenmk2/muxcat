package rabbitmq

import (
	"fmt"
	"regexp"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// superStreamSuffix is the partition naming convention of super streams:
// "<super-stream>-<number>". Matching is a heuristic — a plain stream may
// legitimately be named "foo-1".
var superStreamSuffix = regexp.MustCompile(`^(.+)-\d+$`)

func newStreamCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stream",
		Short: "Inspect streams (requires RabbitMQ >= 3.9)",
		Long: `Inspect streams and the stream protocol. stream ls shows
stream queues with a heuristic SUPER column (partition names
"<super-stream>-<number>" are attributed to their super stream);
stream show gives a queue-view detail of one stream. The connection,
publisher and consumer groups list stream protocol activity.

Requires RabbitMQ >= 3.9 (super streams >= 3.11); on older servers the
endpoints answer 404, reported as UNSUPPORTED_OPERATION.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newStreamLsCmd(),
		newStreamShowCmd(),
		newStreamConnectionCmd(),
		newStreamPublisherCmd(),
		newStreamConsumerCmd(),
	)
	return c
}

// superStreamOf applies the partition-name heuristic.
func superStreamOf(name string) string {
	if m := superStreamSuffix.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// isStream reports whether a queue document is a stream, across versions:
// 4.x carries type=stream, 3.9+ only arguments.x-queue-type=stream.
func isStream(q map[string]any) bool {
	return queueType(q) == "stream"
}

func newStreamLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List streams (GET /api/queues, filtered to type stream)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq stream ls
  muxcat rabbitmq stream ls --vhost /`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := "/api/queues"
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
				q := obj(item)
				if !isStream(q) {
					continue
				}
				rows = append(rows, []any{
					str(q["name"]), str(q["vhost"]), str(q["state"]),
					numOf(q["messages"]), numOf(q["consumers"]),
					megaBytes(q["memory"]), superStreamOf(str(q["name"])),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "vhost", "state", "messages", "consumers", "memory", "super"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	return c
}

func newStreamShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a stream's detail (GET /api/queues/<vhost>/<name>, stream view)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq stream show my-stream
  muxcat rabbitmq stream show my-stream --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/queues/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
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
	c.Flags().String("vhost", "", "vhost of the stream (default: /)")
	return c
}

func newStreamConnectionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "connection",
		Short: "Inspect stream protocol connections (GET /api/stream/connections)",
		Long:  `Inspect stream protocol connections (RabbitMQ >= 3.9).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newStreamListCmd("connection", "connections",
		[]string{"name", "vhost", "user", "state", "node"},
		func(m map[string]any) []any {
			return []any{str(m["name"]), str(m["vhost"]), str(m["user"]), str(m["state"]), str(m["node"])}
		}))
	return c
}

func newStreamPublisherCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "publisher",
		Short: "Inspect stream publishers (GET /api/stream/publishers)",
		Long:  `Inspect stream publishers (RabbitMQ >= 3.9).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newStreamListCmd("publisher", "publishers",
		[]string{"stream", "reference", "id", "connection", "published", "confirmed", "errored"},
		func(m map[string]any) []any {
			return []any{
				str(m["stream"]), str(m["reference"]), numOf(m["publisher_id"]),
				str(obj(m["connection_details"])["name"]),
				numOf(m["messages_published"]), numOf(m["messages_confirmed"]), numOf(m["messages_errored"]),
			}
		}))
	return c
}

func newStreamConsumerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "consumer",
		Short: "Inspect stream consumers (GET /api/stream/consumers)",
		Long:  `Inspect stream consumers (RabbitMQ >= 3.9).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newStreamListCmd("consumer", "consumers",
		[]string{"stream", "subscription_id", "connection", "credits"},
		func(m map[string]any) []any {
			return []any{
				str(m["stream"]), numOf(m["subscription_id"]),
				str(obj(m["connection_details"])["name"]), numOf(m["credits"]),
			}
		}))
	return c
}

// newStreamListCmd builds an ls command over one of the /api/stream/*
// endpoints, all read-only and 3.9+ gated by the client error mapping.
func newStreamListCmd(what, endpoint string, columns []string, rowFn func(map[string]any) []any) *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: fmt.Sprintf("List stream %ss (GET /api/stream/%s)", what, endpoint),
		Args:  cobra.NoArgs,
		Example: fmt.Sprintf(`  muxcat rabbitmq stream %s ls
  muxcat rabbitmq stream %s ls --vhost /`, what, what),
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := "/api/stream/" + endpoint
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
				rows = append(rows, rowFn(obj(item)))
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  columns,
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	return c
}
