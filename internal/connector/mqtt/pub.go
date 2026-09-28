package mqtt

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// publishPayloadFileLimit caps --payload-file at 1MB: publish is a
// debugging facility, not a bulk loader.
const publishPayloadFileLimit = 1 << 20

func newPubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pub <topic>",
		Short: "Publish a message to a topic",
		Long: `Publish a message to an MQTT topic. The payload comes from
--payload, --payload-file (capped at 1MB), or --payload - (stdin);
exactly one source is required. For QoS 1/2 the command returns
after the broker acknowledgment (PUBACK/PUBCOMP). --count repeats
the message; a first-send failure aborts, a mid-loop failure reports
the partial count. pub is a write operation and is refused on
readonly connections before any dialing happens.`,
		Args: cli.ExactArgs(1, "<topic>", "topic"),
		Example: `  muxcat mqtt pub events/boot --payload '{"id":42}'
  muxcat mqtt pub events/tick --payload hi --qos 1 --count 3
  muxcat mqtt pub devices/cam1 --payload-file snapshot.jpg --qos 1
  echo hi | muxcat mqtt pub events/stdin --payload -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			payload, err := resolvePublishPayload(cmd)
			if err != nil {
				return err
			}
			qos, _ := cmd.Flags().GetInt("qos")
			if qos < 0 || qos > 2 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --qos: %d", qos), "valid values: 0 | 1 | 2")
			}
			count, _ := cmd.Flags().GetInt("count")
			if count < 1 || count > 100000 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --count: %d", count), "valid range: 1-100000")
			}
			retain := cli.FlagBool(cmd, "retain")
			env, err := openForCmd(cmd, "pub")
			if err != nil {
				return err
			}
			defer env.close()

			sent := 0
			summary := func() *output.Result {
				value := map[string]any{
					"topic": args[0], "qos": qos, "retain": retain, "sent": sent,
				}
				message := fmt.Sprintf("published to %s (sent: %d, qos: %d, retain: %v)",
					args[0], sent, qos, retain)
				return &output.Result{Value: value, Message: message}
			}
			progress := count >= 100
			for sent < count {
				if err := env.client.Publish(env.ctx, args[0], payload, byte(qos), retain); err != nil {
					if sent > 0 {
						// Partial progress: render the summary, then fail.
						return cli.RenderPartial(cmd, summary(), meta(env.name, start, false), err)
					}
					return err
				}
				sent++
				if progress && sent%100 == 0 {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "published %d/%d...\n", sent, count)
				}
			}
			return cli.RenderResult(cmd, summary(), meta(env.name, start, false))
		},
	}
	c.Flags().String("payload", "", "message payload ('-' reads stdin; mutually exclusive with --payload-file)")
	c.Flags().String("payload-file", "", "read the payload from a file (max 1MB; mutually exclusive with --payload)")
	c.Flags().Int("qos", 0, "MQTT QoS level: 0 | 1 | 2")
	c.Flags().Bool("retain", false, "set the retain flag")
	c.Flags().Int("count", 1, "send the message this many times (1-100000)")
	return c
}

// resolvePublishPayload resolves the payload bytes from --payload or
// --payload-file (mutually exclusive, exactly one required); --payload -
// reads stdin.
func resolvePublishPayload(cmd *cobra.Command) ([]byte, error) {
	payloadSet := cmd.Flags().Changed("payload")
	file := cli.FlagString(cmd, "payload-file")
	if payloadSet && file != "" {
		return nil, output.NewError(output.CodeConfigInvalid,
			"--payload and --payload-file are mutually exclusive", "pass exactly one payload source")
	}
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
		return content, nil
	}
	if !payloadSet {
		return nil, output.NewError(output.CodeMissingArgument,
			"missing payload: pass --payload or --payload-file", "")
	}
	if cli.FlagString(cmd, "payload") == "-" {
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read payload from stdin: "+err.Error(), "")
		}
		return raw, nil
	}
	return []byte(cli.FlagString(cmd, "payload")), nil
}
