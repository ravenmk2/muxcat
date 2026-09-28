package rabbitmq

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// payloadDisplayLimit caps how much of a message payload is rendered in
// text mode; JSONData keeps the raw (possibly base64) payload.
const payloadDisplayLimit = 1024

func newQueueCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "queue",
		Short: "Manage queues",
		Long: `Manage queues: list and inspect them, declare and delete
them, purge their contents, and peek at messages (queue get).

queue get defaults to the non-destructive ack_requeue_true ackmode
(peek semantics); --ackmode ack_requeue_false consumes messages. All
of this goes through the Management API, which is meant for debugging,
not for high-volume consuming.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newQueueLsCmd(),
		newQueueShowCmd(),
		newQueueDeclareCmd(),
		newQueueDeleteCmd(),
		newQueuePurgeCmd(),
		newQueueGetCmd(),
	)
	return c
}

// queueType resolves a queue's type across server versions: 4.x carries a
// "type" field, older servers only arguments.x-queue-type, and a queue
// with neither is classic (the pre-3.8 default).
func queueType(m map[string]any) string {
	if t := str(m["type"]); t != "" {
		return t
	}
	if t := str(obj(m["arguments"])["x-queue-type"]); t != "" {
		return t
	}
	return "classic"
}

func newQueueLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List queues (GET /api/queues)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq queue ls
  muxcat rabbitmq queue ls --vhost /
  muxcat rabbitmq queue ls --json`,
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
				rows = append(rows, []any{
					str(q["name"]), str(q["vhost"]), queueType(q), str(q["state"]),
					numOf(q["messages"]), numOf(q["messages_ready"]),
					numOf(q["messages_unacknowledged"]), numOf(q["consumers"]),
					megaBytes(q["memory"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "vhost", "type", "state", "messages", "ready", "unacked", "consumers", "memory"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	return c
}

func newQueueShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a queue's full detail (GET /api/queues/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq queue show my-queue
  muxcat rabbitmq queue show my-queue --vhost /
  muxcat rabbitmq queue show my-queue --json`,
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
	c.Flags().String("vhost", "", "vhost of the queue (default: /)")
	return c
}

func newQueueDeclareCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "declare <name>",
		Short: "Declare a queue (PUT /api/queues/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq queue declare my-queue
  muxcat rabbitmq queue declare q1 --vhost / --type quorum --durable
  muxcat rabbitmq queue declare q2 --args '{"x-max-length":1000,"x-queue-type":"stream"}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "queue declare"); err != nil {
				return err
			}
			arguments, err := parseJSONArg("--args", cli.FlagString(cmd, "args"))
			if err != nil {
				return err
			}
			if t := cli.FlagString(cmd, "type"); t != "" {
				switch t {
				case "classic", "quorum", "stream":
				default:
					return output.NewError(output.CodeConfigInvalid,
						"invalid --type: "+t, "valid values: classic|quorum|stream")
				}
				arguments["x-queue-type"] = t
			}
			body, err := json.Marshal(map[string]any{
				"durable":     cli.FlagBool(cmd, "durable"),
				"auto_delete": cli.FlagBool(cmd, "auto-delete"),
				"arguments":   arguments,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/queues/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "PUT", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "vhost": vhostFlag(cmd), "declared": true},
				Message: fmt.Sprintf("declared queue %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost to declare the queue in (default: /)")
	c.Flags().Bool("durable", true, "survive a broker restart")
	c.Flags().Bool("auto-delete", false, "delete the queue when its last consumer unsubscribes")
	c.Flags().String("type", "", "queue type: classic|quorum|stream (maps to x-queue-type)")
	c.Flags().String("args", "", "extra queue arguments as a JSON object, merged into arguments")
	return c
}

func newQueueDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete a queue (DELETE /api/queues/<vhost>/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq queue delete my-queue
  muxcat rabbitmq queue delete my-queue --if-empty --if-unused`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "queue delete"); err != nil {
				return err
			}
			path := fmt.Sprintf("/api/queues/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			sep := "?"
			if cli.FlagBool(cmd, "if-empty") {
				path += sep + "if-empty=true"
				sep = "&"
			}
			if cli.FlagBool(cmd, "if-unused") {
				path += sep + "if-unused=true"
			}
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "vhost": vhostFlag(cmd), "deleted": true},
				Message: fmt.Sprintf("deleted queue %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the queue (default: /)")
	c.Flags().Bool("if-empty", false, "refuse to delete a queue that still holds messages")
	c.Flags().Bool("if-unused", false, "refuse to delete a queue that has consumers or recent activity")
	return c
}

func newQueuePurgeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "purge <name>",
		Short: "Purge all messages of a queue (DELETE /api/queues/<vhost>/<name>/contents)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq queue purge my-queue
  muxcat rabbitmq queue purge my-queue --vhost /`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "queue purge"); err != nil {
				return err
			}
			path := fmt.Sprintf("/api/queues/%s/%s/contents", esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "vhost": vhostFlag(cmd), "purged": true},
				Message: fmt.Sprintf("purged queue %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the queue (default: /)")
	return c
}

func newQueueGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <name>",
		Short: "Fetch messages from a queue (POST /api/queues/<vhost>/<name>/get)",
		Long: `Fetch messages from a queue through the Management API. This is
a debugging facility, not a consumer: throughput is low and each call
is synchronous.

The default ackmode ack_requeue_true requeues the messages (peek
semantics, non-destructive) and is allowed on readonly connections.
The destructive ackmodes ack_requeue_false (consume) and
reject_requeue_false (discard) remove the messages from the queue and
are refused on readonly connections. Payloads arriving with
payload_encoding=base64 are decoded for display; text output truncates
each payload at 1KiB (--json keeps the raw payload).`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq queue get my-queue
  muxcat rabbitmq queue get my-queue --limit 5
  muxcat rabbitmq queue get my-queue --ackmode ack_requeue_false`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			limit, _ := cmd.Flags().GetInt("limit")
			if limit < 1 || limit > 50 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --limit: %d", limit), "valid range: 1-50")
			}
			ackmode := cli.FlagString(cmd, "ackmode")
			switch ackmode {
			case "ack_requeue_true", "reject_requeue_true":
				// Peek semantics: messages are requeued, readonly-safe.
			case "ack_requeue_false", "reject_requeue_false":
				// Destructive: messages leave the queue.
				if err := requireWritable(conn, "queue get with --ackmode "+ackmode); err != nil {
					return err
				}
			default:
				return output.NewError(output.CodeConfigInvalid,
					"invalid --ackmode: "+ackmode,
					"valid values: ack_requeue_true|ack_requeue_false|reject_requeue_true|reject_requeue_false")
			}
			body, err := json.Marshal(map[string]any{
				"count":    limit,
				"ackmode":  ackmode,
				"encoding": "auto",
				"truncate": 50000,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/queues/%s/%s/get", esc(vhostFlag(cmd)), esc(args[0]))
			resp, err := cl.do(cmd.Context(), "POST", path, body)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				m := obj(item)
				rows = append(rows, []any{
					str(m["exchange"]), str(m["routing_key"]),
					boolOf(m["redelivered"]), numOf(m["message_count"]),
					displayPayload(m),
				})
			}
			res := &output.Result{
				Columns:  []string{"exchange", "routing_key", "redelivered", "message_count", "payload"},
				Rows:     rows,
				JSONData: arr,
			}
			if file := cli.FlagString(cmd, "file"); file != "" {
				files, err := writePayloadFiles(file, arr)
				if err != nil {
					return err
				}
				// With --file, the files summary is the payload of record:
				// --json reports it instead of the raw message array.
				summary := map[string]any{"files": files, "count": len(files)}
				res.Value = summary
				res.JSONData = summary
				res.Message = fmt.Sprintf("wrote %d payload(s) to %s", len(files), strings.Join(files, ", "))
			}
			return cli.RenderResult(cmd, res, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the queue (default: /)")
	c.Flags().Int("limit", 1, "number of messages to fetch (1-50)")
	c.Flags().String("ackmode", "ack_requeue_true",
		"ack_requeue_true/reject_requeue_true requeue (peek); ack_requeue_false/reject_requeue_false are destructive")
	c.Flags().String("file", "", "write message payloads to disk: <path> for one message, <path>.0, .1... for several (0600)")
	return c
}

// writePayloadFiles writes fetched message payloads to disk: a single
// message goes to path, several to path.0, path.1... base64 payloads are
// decoded to their raw bytes first. Files are created 0600; any failure
// removes the files already written, so a failed run leaves nothing
// behind.
func writePayloadFiles(path string, arr []any) ([]string, error) {
	files := make([]string, 0, len(arr))
	fail := func(err error) ([]string, error) {
		for _, f := range files {
			_ = os.Remove(f)
		}
		return nil, err
	}
	for i, item := range arr {
		m := obj(item)
		payload := str(m["payload"])
		var raw []byte
		if str(m["payload_encoding"]) == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return fail(output.NewError(output.CodeQueryError,
					fmt.Sprintf("message %d has invalid base64 payload: %v", i, err), ""))
			}
			raw = decoded
		} else {
			raw = []byte(payload)
		}
		target := path
		if len(arr) > 1 {
			target = fmt.Sprintf("%s.%d", path, i)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			return fail(output.NewError(output.CodeGeneral,
				"cannot write "+target+": "+err.Error(), ""))
		}
		files = append(files, target)
	}
	return files, nil
}

// displayPayload decodes a base64 payload and truncates the display text
// at 1KiB with a marker. The raw payload (with its encoding) stays in
// JSONData.
func displayPayload(m map[string]any) string {
	payload := str(m["payload"])
	if str(m["payload_encoding"]) == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(payload); err == nil {
			payload = string(raw)
		}
	}
	if len(payload) > payloadDisplayLimit {
		return fmt.Sprintf("%s... (truncated, %d bytes total)", payload[:payloadDisplayLimit], len(payload))
	}
	return payload
}
