package rabbitmq

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newExchangeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "exchange",
		Short: "Manage exchanges",
		Long: `Manage exchanges: list and inspect them, declare and delete
them, and publish a message (exchange publish).

exchange publish goes through the Management API and is meant for
debugging only — it is synchronous, unconfirmed, and not suited for
large payloads or high rates.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newExchangeLsCmd(),
		newExchangeShowCmd(),
		newExchangeDeclareCmd(),
		newExchangeDeleteCmd(),
		newExchangePublishCmd(),
	)
	return c
}

func newExchangeLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List exchanges (GET /api/exchanges)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq exchange ls
  muxcat rabbitmq exchange ls --vhost /`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := "/api/exchanges"
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
				e := obj(item)
				rows = append(rows, []any{
					str(e["name"]), str(e["vhost"]), str(e["type"]),
					boolOf(e["durable"]), boolOf(e["auto_delete"]),
					boolOf(e["internal"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "vhost", "type", "durable", "auto_delete", "internal"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	return c
}

func newExchangeShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show an exchange's full detail (GET /api/exchanges/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq exchange show amq.direct
  muxcat rabbitmq exchange show amq.direct --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/exchanges/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
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
	c.Flags().String("vhost", "", "vhost of the exchange (default: /)")
	return c
}

func newExchangeDeclareCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "declare <name>",
		Short: "Declare an exchange (PUT /api/exchanges/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq exchange declare events --type topic
  muxcat rabbitmq exchange declare fan --type fanout --durable`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "exchange declare"); err != nil {
				return err
			}
			exType := cli.FlagString(cmd, "type")
			switch exType {
			case "direct", "fanout", "topic", "headers":
			default:
				return output.NewError(output.CodeConfigInvalid,
					"invalid --type: "+exType, "valid values: direct|fanout|topic|headers")
			}
			arguments, err := parseJSONArg("--args", cli.FlagString(cmd, "args"))
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{
				"type":        exType,
				"durable":     cli.FlagBool(cmd, "durable"),
				"auto_delete": cli.FlagBool(cmd, "auto-delete"),
				"arguments":   arguments,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/exchanges/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "PUT", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"exchange": args[0], "vhost": vhostFlag(cmd), "declared": true},
				Message: fmt.Sprintf("declared exchange %s (%s) in vhost %s", args[0], exType, vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost to declare the exchange in (default: /)")
	c.Flags().String("type", "direct", "exchange type: direct|fanout|topic|headers")
	c.Flags().Bool("durable", true, "survive a broker restart")
	c.Flags().Bool("auto-delete", false, "delete the exchange when its last queue is unbound")
	c.Flags().String("args", "", "extra exchange arguments as a JSON object, merged into arguments")
	return c
}

func newExchangeDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete an exchange (DELETE /api/exchanges/<vhost>/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq exchange delete events
  muxcat rabbitmq exchange delete events --if-unused`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "exchange delete"); err != nil {
				return err
			}
			path := fmt.Sprintf("/api/exchanges/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			if cli.FlagBool(cmd, "if-unused") {
				path += "?if-unused=true"
			}
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"exchange": args[0], "vhost": vhostFlag(cmd), "deleted": true},
				Message: fmt.Sprintf("deleted exchange %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the exchange (default: /)")
	c.Flags().Bool("if-unused", false, "refuse to delete an exchange that still has bindings")
	return c
}

func newExchangePublishCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "publish <name>",
		Short: "Publish a message to an exchange (POST /api/exchanges/<vhost>/<name>/publish)",
		Long: `Publish a message to an exchange through the Management API.
This is a debugging facility: it is synchronous, unconfirmed, and not
suited for large payloads or high rates. The result reports whether
the message was routed to at least one queue.

The payload comes from --payload or --payload-file (mutually
exclusive; the file is capped at 1MB). --count sends the same message
repeatedly and summarizes how many were sent and routed; the first
failure stops the loop and reports the partial count.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq exchange publish events --routing-key app.started --payload '{"id":42}'
  muxcat rabbitmq exchange publish amq.direct --payload aGVsbG8= --payload-encoding base64
  muxcat rabbitmq exchange publish events --payload-file message.bin --count 100
  muxcat rabbitmq exchange publish events --payload hi --props '{"content_type":"text/plain","delivery_mode":2}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "exchange publish"); err != nil {
				return err
			}
			encoding := cli.FlagString(cmd, "payload-encoding")
			switch encoding {
			case "string", "base64":
			default:
				return output.NewError(output.CodeConfigInvalid,
					"invalid --payload-encoding: "+encoding, "valid values: string|base64")
			}
			payload, err := resolvePublishPayload(cmd, encoding)
			if err != nil {
				return err
			}
			count, _ := cmd.Flags().GetInt("count")
			if count < 1 || count > 10000 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --count: %d", count), "valid range: 1-10000")
			}
			props, err := parseJSONArg("--props", cli.FlagString(cmd, "props"))
			if err != nil {
				return err
			}
			routingKey := cli.FlagString(cmd, "routing-key")
			body, err := json.Marshal(map[string]any{
				"properties":       props,
				"routing_key":      routingKey,
				"payload":          payload,
				"payload_encoding": encoding,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/exchanges/%s/%s/publish", esc(vhostFlag(cmd)), esc(args[0]))

			sent, routed := 0, 0
			summary := func() *output.Result {
				return &output.Result{
					Value: map[string]any{
						"exchange": args[0], "routing_key": routingKey,
						"sent": sent, "routed": routed,
					},
					Message: fmt.Sprintf("published to %s (sent: %d, routed: %d)", args[0], sent, routed),
				}
			}
			for sent < count {
				resp, err := cl.do(cmd.Context(), "POST", path, body)
				if err != nil {
					if sent > 0 {
						// Partial progress: render the summary, then fail.
						return cli.RenderPartial(cmd, summary(), meta(name, start, false), err)
					}
					return err
				}
				sent++
				raw, derr := decodeBody(resp.body)
				if derr != nil {
					return cli.RenderPartial(cmd, summary(), meta(name, start, false), derr)
				}
				if boolOf(obj(raw)["routed"]) {
					routed++
				}
			}
			res := summary()
			if routed == 0 {
				res.Message = fmt.Sprintf("published to %s (sent: %d), but no queue was bound (routed: 0)", args[0], sent)
			}
			return cli.RenderResult(cmd, res, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the exchange (default: /)")
	c.Flags().String("routing-key", "", "routing key of the message")
	c.Flags().String("payload", "", "message payload (mutually exclusive with --payload-file)")
	c.Flags().String("payload-file", "", "read the payload from a file (max 1MB; mutually exclusive with --payload)")
	c.Flags().String("payload-encoding", "string", "payload encoding: string|base64 (file content is base64-encoded when base64)")
	c.Flags().Int("count", 1, "send the message this many times (1-10000)")
	c.Flags().String("props", "", "message properties as a JSON object (e.g. '{\"delivery_mode\":2}')")
	return c
}

// publishPayloadFileLimit caps --payload-file at 1MB: the Management API
// publish endpoint is a debugging facility, not a bulk loader.
const publishPayloadFileLimit = 1 << 20

// resolvePublishPayload resolves the payload from --payload or
// --payload-file (mutually exclusive, exactly one required). With base64
// encoding, file content is encoded; a --payload string is sent as-is.
func resolvePublishPayload(cmd *cobra.Command, encoding string) (string, error) {
	payloadSet := cmd.Flags().Changed("payload")
	file := cli.FlagString(cmd, "payload-file")
	if payloadSet && file != "" {
		return "", output.NewError(output.CodeConfigInvalid,
			"--payload and --payload-file are mutually exclusive", "pass exactly one payload source")
	}
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read payload file "+file+": "+err.Error(), "")
		}
		if len(raw) > publishPayloadFileLimit {
			return "", output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("payload file too large: %d bytes (limit 1MB)", len(raw)),
				"the Management API publish endpoint is meant for small debug messages")
		}
		if encoding == "base64" {
			return base64.StdEncoding.EncodeToString(raw), nil
		}
		return string(raw), nil
	}
	if !payloadSet {
		return "", output.NewError(output.CodeMissingArgument,
			"missing payload: pass --payload or --payload-file",
			"the Management API publish endpoint requires a payload")
	}
	return cli.FlagString(cmd, "payload"), nil
}
