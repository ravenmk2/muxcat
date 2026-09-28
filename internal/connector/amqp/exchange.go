package amqp

import (
	"encoding/base64"
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
		Short: "Manage exchanges and publish messages",
		Long: `Manage exchanges over the AMQP wire protocol: declare and delete
them, probe them (exchange show), and publish messages
(exchange publish) with native confirms (0.9.1) / settlement
outcomes (1.0).

Neither AMQP 0.9.1 nor 1.0 has a list primitive, so there is no
exchange ls — use rmq exchange ls (Management API) for listings.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newExchangeDeclareCmd(),
		newExchangeDeleteCmd(),
		newExchangeShowCmd(),
		newExchangePublishCmd(),
	)
	return c
}

func newExchangeDeclareCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "declare <name>",
		Short: "Declare an exchange",
		Long: `Declare an exchange. AMQP 1.0 has no durability knob (exchanges
are declared durable by the broker), so --durable only applies to
0.9.1.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp exchange declare events --type topic
  muxcat amqp exchange declare fan --type fanout`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "exchange declare")
			if err != nil {
				return err
			}
			defer env.close()
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
			spec := ExchangeSpec{
				Name:       args[0],
				Type:       exType,
				Durable:    cli.FlagBool(cmd, "durable"),
				AutoDelete: cli.FlagBool(cmd, "auto-delete"),
				Args:       arguments,
			}
			if err := env.session.DeclareExchange(env.ctx, spec); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"exchange": args[0], "declared": true},
				Message: fmt.Sprintf("declared exchange %s (%s) in vhost %s", args[0], exType, env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().String("type", "direct", "exchange type: direct|fanout|topic|headers")
	c.Flags().Bool("durable", true, "survive a broker restart (0.9.1 only; 1.0 declares durable exchanges)")
	c.Flags().Bool("auto-delete", false, "delete the exchange when its last queue is unbound")
	c.Flags().String("args", "", "extra exchange arguments as a JSON object")
	return c
}

func newExchangeDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete an exchange",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp exchange delete events
  muxcat amqp exchange delete events --if-unused`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "exchange delete")
			if err != nil {
				return err
			}
			defer env.close()
			if err := env.session.DeleteExchange(env.ctx, args[0], cli.FlagBool(cmd, "if-unused")); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"exchange": args[0], "deleted": true},
				Message: fmt.Sprintf("deleted exchange %s in vhost %s", args[0], env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().Bool("if-unused", false, "refuse to delete an exchange that still has bindings (0.9.1 only)")
	return c
}

func newExchangeShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Probe an exchange (existence and type)",
		Long: `Probe an exchange's existence (0.9.1: passive declare; type is
not reported). Not available over AMQP 1.0: the 1.0 management
interface cannot query exchanges — use rmq exchange show, or a
0.9.1 connection.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp exchange show amq.direct
  muxcat amqp exchange show events --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "")
			if err != nil {
				return err
			}
			defer env.close()
			info, err := env.session.ExchangeInfo(env.ctx, args[0])
			if err != nil {
				return err
			}
			value := map[string]any{"exchange": info.Name, "exists": true}
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

// publishPayloadFileLimit caps --payload-file at 1MB: publish is a
// debugging facility, not a bulk loader.
const publishPayloadFileLimit = 1 << 20

func newExchangePublishCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "publish <name>",
		Short: "Publish a message to an exchange (with broker confirmation)",
		Long: `Publish a message to an exchange over the wire protocol. Every
message is broker-confirmed: publisher confirms on 0.9.1
(mandatory returns count as unroutable), settlement outcomes on
1.0 (released counts as unroutable; rejected is reported with the
broker's queue+reason on RabbitMQ 4.3+).

The payload comes from --payload or --payload-file (mutually
exclusive, exactly one required; the file is capped at 1MB).
--encoding base64 decodes the payload before sending. --persistent
sets delivery_mode=2 / the durable header. --props takes a JSON
object from a portable whitelist (message_id, correlation_id,
content_type, content_encoding, reply_to, type, expiration,
priority, timestamp, user_id, app_id — the last is 0.9.1-only and
dropped with a warning on 1.0); unknown keys are rejected.
--headers k=v,... sets custom business headers (0.9.1 headers
table / 1.0 application-properties). --count repeats the message;
a first-send failure aborts, a mid-loop failure reports the partial
count.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat amqp exchange publish events --routing-key app.started --payload '{"id":42}'
  muxcat amqp exchange publish events --routing-key app.tick --payload hi --count 3 --persistent
  muxcat amqp exchange publish events --payload aGVsbG8= --encoding base64
  muxcat amqp exchange publish events --payload hi --headers tenant=eu,trace=1 --props '{"content_type":"text/plain","message_id":"m-1"}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "exchange publish")
			if err != nil {
				return err
			}
			defer env.close()
			encoding := cli.FlagString(cmd, "encoding")
			switch encoding {
			case "string", "base64":
			default:
				return output.NewError(output.CodeConfigInvalid,
					"invalid --encoding: "+encoding, "valid values: string|base64")
			}
			payload, err := resolvePublishPayload(cmd, encoding)
			if err != nil {
				return err
			}
			count, _ := cmd.Flags().GetInt("count")
			if count < 1 || count > 100000 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --count: %d", count), "valid range: 1-100000")
			}
			props, err := parseJSONArg("--props", cli.FlagString(cmd, "props"))
			if err != nil {
				return err
			}
			if err := validateProps(props); err != nil {
				return err
			}
			if env.inst.protocol() == Protocol10 {
				if _, ok := props["app_id"]; ok {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"Warning: --props app_id has no AMQP 1.0 counterpart and is dropped")
					delete(props, "app_id")
				}
			}
			headers, err := parseHeaders(cli.FlagString(cmd, "headers"))
			if err != nil {
				return err
			}
			routingKey := cli.FlagString(cmd, "routing-key")
			msg := Outgoing{
				Body:       payload,
				Persistent: cli.FlagBool(cmd, "persistent"),
				Props:      props,
				Headers:    headers,
			}

			sent, confirmed, unroutable := 0, 0, 0
			rejected := map[string]int{}
			summary := func() *output.Result {
				value := map[string]any{
					"exchange": args[0], "routing_key": routingKey,
					"sent": sent, "confirmed": confirmed, "unroutable": unroutable,
				}
				message := fmt.Sprintf("published to %s (sent: %d, confirmed: %d, unroutable: %d)",
					args[0], sent, confirmed, unroutable)
				if len(rejected) > 0 {
					value["rejected"] = rejected
					message += fmt.Sprintf(", rejected: %d", sumValues(rejected))
				}
				return &output.Result{Value: value, Message: message}
			}
			progress := count >= 100
			for sent < count {
				oc, err := env.session.Publish(env.ctx, args[0], routingKey, msg)
				if err != nil {
					if sent > 0 {
						// Partial progress: render the summary, then fail.
						return cli.RenderPartial(cmd, summary(), meta(env.name, start, false), err)
					}
					return err
				}
				sent++
				if oc.Confirmed {
					confirmed++
				}
				if oc.Unroutable {
					unroutable++
				}
				if oc.RejectReason != "" {
					rejected[oc.RejectReason]++
				}
				if progress && sent%100 == 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "published %d/%d...\n", sent, count)
				}
			}
			return cli.RenderResult(cmd, summary(), meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().String("routing-key", "", "routing key of the message")
	c.Flags().String("payload", "", "message payload (mutually exclusive with --payload-file)")
	c.Flags().String("payload-file", "", "read the payload from a file (max 1MB; mutually exclusive with --payload)")
	c.Flags().String("encoding", "string", "payload encoding: string|base64 (base64 decodes the input before sending)")
	c.Flags().Int("count", 1, "send the message this many times (1-100000)")
	c.Flags().Bool("persistent", false, "mark messages persistent (delivery_mode=2 / durable header)")
	c.Flags().String("headers", "", "custom message headers as k=v,... pairs")
	c.Flags().String("props", "", "standard message properties as a JSON object (whitelisted subset)")
	return c
}

func sumValues(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// resolvePublishPayload resolves the payload bytes from --payload or
// --payload-file (mutually exclusive, exactly one required). With base64
// encoding the input is decoded to raw bytes.
func resolvePublishPayload(cmd *cobra.Command, encoding string) ([]byte, error) {
	payloadSet := cmd.Flags().Changed("payload")
	file := cli.FlagString(cmd, "payload-file")
	if payloadSet && file != "" {
		return nil, output.NewError(output.CodeConfigInvalid,
			"--payload and --payload-file are mutually exclusive", "pass exactly one payload source")
	}
	var raw []byte
	if file != "" {
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read payload file "+file+": "+err.Error(), "")
		}
		if len(content) > publishPayloadFileLimit {
			return nil, output.NewError(output.CodeConfigInvalid,
				fmt.Sprintf("payload file too large: %d bytes (limit 1MB)", len(content)), "")
		}
		raw = content
	} else {
		if !payloadSet {
			return nil, output.NewError(output.CodeMissingArgument,
				"missing payload: pass --payload or --payload-file", "")
		}
		raw = []byte(cli.FlagString(cmd, "payload"))
	}
	if encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			return nil, output.NewError(output.CodeConfigInvalid,
				"payload is not valid base64: "+err.Error(), "")
		}
		return decoded, nil
	}
	return raw, nil
}
