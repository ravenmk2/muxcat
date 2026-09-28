package emqx

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newPubCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pub <topic>",
		Short: "Publish a message; blocked on readonly connections",
		Long: `Publish a message to a topic. The payload comes from exactly
one of --payload or --file (- reads stdin); a non-UTF-8 payload is sent
base64-encoded (payload_encoding=base64). A 202 answer carrying
{"message":"no_matching_subscribers","reason_code":16} is a normal
result (no subscriber matched) and is rendered as data, not an error.
Blocked on readonly connections.`,
		Args: cli.ExactArgs(1, "<topic>", "topic"),
		Example: `  muxcat emqx pub sensors/temp --payload '{"t":21.5}' --qos 1
  muxcat emqx pub retained/cfg --file cfg.json --retain
  echo hello | muxcat emqx pub dev/echo --payload - --qos 0`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "pub"); err != nil {
				return err
			}
			payload, err := resolvePayload(cmd)
			if err != nil {
				return err
			}
			qos, _ := cmd.Flags().GetInt("qos")
			if qos < 0 || qos > 2 {
				return output.NewError(output.CodeMissingArgument,
					fmt.Sprintf("invalid --qos value: %d", qos), "valid values: 0|1|2")
			}
			topic := args[0]
			// EMQX validates a plain payload as UTF-8 binary(); non-UTF-8
			// payloads go base64-encoded instead (Go's json would otherwise
			// silently mangle them into U+FFFD).
			payloadEnc := "plain"
			if !utf8.ValidString(payload) {
				payload = base64.StdEncoding.EncodeToString([]byte(payload))
				payloadEnc = "base64"
			}
			body, _ := json.Marshal(map[string]any{
				"topic":            topic,
				"payload":          payload,
				"qos":              qos,
				"retain":           cli.FlagBool(cmd, "retain"),
				"payload_encoding": payloadEnc,
			})
			r, err := cl.send(cmd.Context(), http.MethodPost, "/api/v5/publish", nil, body, false)
			if err != nil {
				return err
			}
			data := map[string]any{"topic": topic, "published": true, "status": r.status, "payload_encoding": payloadEnc}
			// 202 may carry {"message":"no_matching_subscribers",
			// "reason_code":16}: a normal result, rendered as data.
			if len(r.body) > 0 {
				var v map[string]any
				if json.Unmarshal(r.body, &v) == nil {
					for k, val := range v {
						data[k] = val
					}
				}
			}
			msg := fmt.Sprintf("published to %s (HTTP %d)", topic, r.status)
			if m, ok := data["message"].(string); ok && m != "" {
				msg = fmt.Sprintf("published to %s (HTTP %d, %s)", topic, r.status, m)
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  msg,
				JSONData: data,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("payload", "", "message payload as a literal string (- reads stdin)")
	c.Flags().String("file", "", "read the payload from a file (- reads stdin)")
	c.Flags().Int("qos", 0, "QoS level: 0|1|2")
	c.Flags().Bool("retain", false, "publish as a retained message")
	return c
}

// resolvePayload reads the publish payload: exactly one of --payload or
// --file is required; "-" reads stdin.
func resolvePayload(cmd *cobra.Command) (string, error) {
	payload := cli.FlagString(cmd, "payload")
	file := cli.FlagString(cmd, "file")
	switch {
	case payload != "" && file != "":
		return "", output.NewError(output.CodeMissingArgument,
			"--payload and --file are mutually exclusive", "pass exactly one payload source")
	case payload == "-", file == "-":
		if cli.RuntimeFrom(cmd.Context()).TTY {
			return "", output.NewError(output.CodeMissingArgument,
				"- reads the payload from stdin, but stdin is a terminal",
				"pipe the payload in, or pass a literal string / a file path")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read payload from stdin: "+err.Error(), "")
		}
		return string(raw), nil
	case payload != "":
		return payload, nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read payload file "+file+": "+err.Error(), "")
		}
		return string(raw), nil
	default:
		return "", output.NewError(output.CodeMissingArgument,
			"missing payload: pass --payload <string> or --file <path>",
			"example: muxcat emqx pub sensors/temp --payload hello")
	}
}
