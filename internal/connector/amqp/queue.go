package amqp

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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
		Long: `Manage queues over the AMQP wire protocol: declare and delete
them, purge their contents, inspect them (queue show), and pull a
single message for debugging (queue get).

Neither AMQP 0.9.1 nor 1.0 has a list primitive, so there is no
queue ls — use rmq queue ls (Management API) for listings.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newQueueDeclareCmd(),
		newQueueDeleteCmd(),
		newQueuePurgeCmd(),
		newQueueShowCmd(),
		newQueueGetCmd(),
	)
	return c
}

func newQueueDeclareCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "declare <name>",
		Short: "Declare a queue",
		Long: `Declare a queue. --durable defaults to true: RabbitMQ 4.x refuses
non-durable non-exclusive transient queues as a deprecated feature.
--type classic|quorum|stream maps to a typed spec on AMQP 1.0 and to
the x-queue-type argument on 0.9.1. quorum and stream queues are
always durable and reject --auto-delete/--exclusive. AMQP 1.0 has no
durability knob of its own (classic queues are declared durable by
the broker), so --durable only applies to 0.9.1.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp queue declare my-queue
  muxcat amqp queue declare q1 --type quorum
  muxcat amqp queue declare q2 --args '{"x-max-length":1000}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "queue declare")
			if err != nil {
				return err
			}
			defer env.close()
			spec := QueueSpec{
				Name:       args[0],
				Durable:    cli.FlagBool(cmd, "durable"),
				AutoDelete: cli.FlagBool(cmd, "auto-delete"),
				Exclusive:  cli.FlagBool(cmd, "exclusive"),
			}
			if t := cli.FlagString(cmd, "type"); t != "" {
				switch t {
				case "classic", "quorum", "stream":
					spec.Type = t
				default:
					return output.NewError(output.CodeConfigInvalid,
						"invalid --type: "+t, "valid values: classic|quorum|stream")
				}
			}
			if (spec.Type == "quorum" || spec.Type == "stream") && (spec.AutoDelete || spec.Exclusive) {
				return output.NewError(output.CodeConfigInvalid,
					"--auto-delete/--exclusive are not supported by "+spec.Type+" queues",
					"quorum and stream queues are always durable and never exclusive")
			}
			spec.Args, err = parseJSONArg("--args", cli.FlagString(cmd, "args"))
			if err != nil {
				return err
			}
			if err := env.session.DeclareQueue(env.ctx, spec); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "declared": true},
				Message: fmt.Sprintf("declared queue %s in vhost %s", args[0], env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().Bool("durable", true, "survive a broker restart (0.9.1 only; 1.0 declares durable queues)")
	c.Flags().Bool("auto-delete", false, "delete the queue when its last consumer unsubscribes")
	c.Flags().Bool("exclusive", false, "restrict the queue to this connection")
	c.Flags().String("type", "", "queue type: classic|quorum|stream (maps to x-queue-type on 0.9.1)")
	c.Flags().String("args", "", "extra queue arguments as a JSON object")
	return c
}

func newQueueDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete a queue",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp queue delete my-queue
  muxcat amqp queue delete my-queue --if-empty --if-unused`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "queue delete")
			if err != nil {
				return err
			}
			defer env.close()
			err = env.session.DeleteQueue(env.ctx, args[0],
				cli.FlagBool(cmd, "if-empty"), cli.FlagBool(cmd, "if-unused"))
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "deleted": true},
				Message: fmt.Sprintf("deleted queue %s in vhost %s", args[0], env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().Bool("if-empty", false, "refuse to delete a queue that still holds messages")
	c.Flags().Bool("if-unused", false, "refuse to delete a queue that has consumers")
	return c
}

func newQueuePurgeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "purge <name>",
		Short: "Purge all messages of a queue",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp queue purge my-queue
  muxcat amqp queue purge my-queue --vhost /prod`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "queue purge")
			if err != nil {
				return err
			}
			defer env.close()
			n, err := env.session.PurgeQueue(env.ctx, args[0])
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"queue": args[0], "purged": n},
				Message: fmt.Sprintf("purged queue %s in vhost %s (%d messages)", args[0], env.vhost, n),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	return c
}

func newQueueShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a queue (messages, consumers; type on AMQP 1.0)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp queue show my-queue
  muxcat amqp queue show my-queue --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "")
			if err != nil {
				return err
			}
			defer env.close()
			info, err := env.session.QueueInfo(env.ctx, args[0])
			if err != nil {
				return err
			}
			value := map[string]any{
				"queue": info.Name, "messages": info.Messages, "consumers": info.Consumers,
			}
			if info.Type != "" {
				value["type"] = info.Type
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:  value,
				Syntax: "yaml",
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	return c
}

func newQueueGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <name>",
		Short: "Pull a single message from a queue (debugging facility)",
		Long: `Pull a single message from a queue. The default --ackmode peek
requeues the message (0.9.1: get + nack/requeue, 1.0: receive +
release) — non-destructive and allowed on readonly connections.
--ackmode ack (consume) and --ackmode reject (discard/dead-letter)
are destructive and refused on readonly connections. Text output
truncates the payload at 1KiB; --json keeps the full payload
(base64 for non-UTF-8 bodies); --file writes the payload to disk
(0600).`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp queue get my-queue
  muxcat amqp queue get my-queue --ackmode ack
  muxcat amqp queue get my-queue --file payload.bin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			ackmode := cli.FlagString(cmd, "ackmode")
			writeOp := ""
			switch ackmode {
			case "peek":
			case "ack", "reject":
				writeOp = "queue get with --ackmode " + ackmode
			default:
				return output.NewError(output.CodeConfigInvalid,
					"invalid --ackmode: "+ackmode, "valid values: peek|ack|reject")
			}
			env, err := openForCmd(cmd, writeOp)
			if err != nil {
				return err
			}
			defer env.close()
			dc, err := env.session.Get(env.ctx, args[0], ackmode)
			if err != nil {
				return err
			}
			if dc == nil {
				return cli.RenderResult(cmd, &output.Result{
					Value:   map[string]any{"queue": args[0], "message": nil},
					Message: fmt.Sprintf("queue %s is empty", args[0]),
				}, meta(env.name, start, false))
			}
			msg := dc.Delivery()
			data := messageData(msg)
			res := &output.Result{
				Columns:  []string{"exchange", "routing_key", "redelivered", "payload"},
				Rows:     [][]any{{msg.Exchange, msg.RoutingKey, msg.Redelivered, displayPayload(msg.Payload)}},
				JSONData: data,
			}
			if file := cli.FlagString(cmd, "file"); file != "" {
				if err := os.WriteFile(file, msg.Payload, 0o600); err != nil {
					return output.NewError(output.CodeGeneral,
						"cannot write "+file+": "+err.Error(), "")
				}
				summary := map[string]any{"file": file, "bytes": len(msg.Payload)}
				res.Value = summary
				res.JSONData = summary
				res.Rows = nil
				res.Columns = nil
				res.Message = fmt.Sprintf("wrote %d bytes to %s", len(msg.Payload), file)
			}
			return cli.RenderResult(cmd, res, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().String("ackmode", "peek", "peek requeues (non-destructive); ack consumes; reject discards/dead-letters")
	c.Flags().String("file", "", "write the message payload to this file (0600)")
	return c
}

// settle applies the ackmode to a delivery.
func settle(ctx context.Context, dc DeliveryContext, mode string) error {
	var err error
	switch mode {
	case "ack":
		err = dc.Ack(ctx)
	case "reject":
		err = dc.Reject(ctx)
	default: // peek / requeue
		err = dc.Requeue(ctx)
	}
	if err != nil {
		return output.NewError(output.CodeQueryError, "failed to settle message ("+mode+"): "+err.Error(), "")
	}
	return nil
}

// messageData builds the JSON view of a message: UTF-8 payloads travel as
// strings, binary payloads base64 with payload_encoding.
func messageData(m *Delivery) map[string]any {
	data := map[string]any{
		"exchange":    m.Exchange,
		"routing_key": m.RoutingKey,
		"redelivered": m.Redelivered,
	}
	if utf8.Valid(m.Payload) {
		data["payload"] = string(m.Payload)
	} else {
		data["payload"] = base64.StdEncoding.EncodeToString(m.Payload)
		data["payload_encoding"] = "base64"
	}
	if len(m.Properties) > 0 {
		data["properties"] = m.Properties
	}
	return data
}

// displayPayload truncates the display text at 1KiB with a marker;
// non-UTF-8 bodies are reported by size.
func displayPayload(payload []byte) string {
	if !utf8.Valid(payload) {
		return fmt.Sprintf("(binary, %d bytes, base64 in --json)", len(payload))
	}
	s := string(payload)
	if len(s) > payloadDisplayLimit {
		return fmt.Sprintf("%s... (truncated, %d bytes total)", s[:payloadDisplayLimit], len(s))
	}
	return s
}

// writePayloads writes message payloads into a directory, one file per
// message (0600). Any failure removes the files already written.
func writePayloads(dir, queue string, msgs []*Delivery) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, output.NewError(output.CodeGeneral,
			"cannot create directory "+dir+": "+err.Error(), "")
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, queue)
	files := make([]string, 0, len(msgs))
	fail := func(err error) ([]string, error) {
		for _, f := range files {
			_ = os.Remove(f)
		}
		return nil, err
	}
	for i, m := range msgs {
		target := fmt.Sprintf("%s%c%s-%d.bin", dir, os.PathSeparator, safe, i)
		if err := os.WriteFile(target, m.Payload, 0o600); err != nil {
			return fail(output.NewError(output.CodeGeneral,
				"cannot write "+target+": "+err.Error(), ""))
		}
		files = append(files, target)
	}
	return files, nil
}
