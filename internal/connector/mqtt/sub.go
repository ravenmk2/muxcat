package mqtt

import (
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

// payloadDisplayLimit truncates text payloads at 1KiB in non-JSON output.
const payloadDisplayLimit = 1024

func newSubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sub <topic-filter>",
		Short: "Subscribe to a topic filter (one-shot batch)",
		Long: `Subscribe to an MQTT topic filter in one-shot mode: collect up
to --count messages or stop at --timeout, and return one envelope.
Text output truncates each payload at 1KiB; --json keeps full
payloads (base64 for non-UTF-8 bodies); --file writes payloads to a
directory (0600). Receiving no message before the timeout fails with
TIMEOUT (exit 3). A streaming --follow mode is not implemented.

The session is clean-start with a random client id, so the batch
only sees messages published inside the subscription window (plus
retained messages).`,
		Args: cli.ExactArgs(1, "<topic-filter>", "topic filter"),
		Example: `  muxcat mqtt sub events/#
  muxcat mqtt sub sensors/+/temp --count 10 --timeout 10s
  muxcat mqtt sub 'devices/#' --qos 1 --json
  muxcat mqtt sub blobs/# --count 5 --file ./payloads`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			count, _ := cmd.Flags().GetInt("count")
			if count < 1 || count > 10000 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --count: %d", count), "valid range: 1-10000")
			}
			wait, _ := cmd.Flags().GetDuration("timeout")
			if wait <= 0 {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --timeout: "+wait.String(), "examples: 5s, 1m (must be > 0)")
			}
			qos, _ := cmd.Flags().GetInt("qos")
			if qos < 0 || qos > 2 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --qos: %d", qos), "valid values: 0 | 1 | 2")
			}
			env, err := openForCmd(cmd, "")
			if err != nil {
				return err
			}
			defer env.close()

			// The buffer holds the whole batch; the handler never blocks the
			// client read loop (late messages past --count are dropped).
			received := make(chan Message, count)
			err = env.client.Subscribe(env.ctx, args[0], byte(qos), func(m Message) {
				select {
				case received <- m:
				default:
				}
			})
			if err != nil {
				return err
			}

			timer := time.NewTimer(wait)
			defer timer.Stop()
			var collected []Message
		collect:
			for len(collected) < count {
				select {
				case m := <-received:
					collected = append(collected, m)
				case <-timer.C:
					break collect
				}
			}
			if len(collected) == 0 {
				return output.NewError(output.CodeTimeout,
					fmt.Sprintf("no message received on %s within %s", args[0], wait),
					"widen --timeout, or check the topic filter and that publishers are active")
			}

			rows := make([][]any, 0, len(collected))
			jsonRows := make([]any, 0, len(collected))
			for _, m := range collected {
				rows = append(rows, []any{m.Topic, m.QoS, m.Retained, displayPayload(m.Payload)})
				jsonRows = append(jsonRows, messageData(m))
			}
			res := &output.Result{
				Columns:  []string{"topic", "qos", "retained", "payload"},
				Rows:     rows,
				JSONData: jsonRows,
			}
			if dir := cli.FlagString(cmd, "file"); dir != "" {
				files, err := writePayloads(dir, args[0], collected)
				if err != nil {
					return err
				}
				summary := map[string]any{"files": files, "count": len(files)}
				res.Value = summary
				res.JSONData = summary
				res.Rows = nil
				res.Columns = nil
				res.Message = fmt.Sprintf("wrote %d payload(s) to %s", len(files), strings.Join(files, ", "))
			}
			return cli.RenderResult(cmd, res, meta(env.name, start, false))
		},
	}
	c.Flags().Int("count", 1, "number of messages to collect (1-10000)")
	c.Flags().Duration("timeout", 30*time.Second, "stop collecting after this long without enough messages")
	c.Flags().Int("qos", 1, "subscription QoS level: 0 | 1 | 2")
	c.Flags().String("file", "", "write payloads to this directory: <filter>-0.bin, <filter>-1.bin... (0600)")
	return c
}

// messageData builds the JSON view of a message: UTF-8 payloads travel as
// strings, binary payloads base64 with payload_encoding.
func messageData(m Message) map[string]any {
	data := map[string]any{
		"topic":    m.Topic,
		"qos":      m.QoS,
		"retained": m.Retained,
	}
	if utf8.Valid(m.Payload) {
		data["payload"] = string(m.Payload)
	} else {
		data["payload"] = base64.StdEncoding.EncodeToString(m.Payload)
		data["payload_encoding"] = "base64"
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
func writePayloads(dir, filter string, msgs []Message) ([]string, error) {
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
	}, filter)
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
