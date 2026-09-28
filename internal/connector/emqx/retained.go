package emqx

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// retainedPayloadMax is the payload truncation limit of retained show
// (4KB), guarding against large binary payloads flooding the terminal.
const retainedPayloadMax = 4 << 10

func newRetainedCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "retained",
		Short: "Inspect retained messages",
		Long: `Inspect retained messages. retained ls lists topics holding a
retained message (paginated); retained show prints one message (the
payload is base64-decoded for display, kept base64 with a note when not
UTF-8 text, and truncated to 4KB); retained rm deletes a retained
message and is blocked on readonly connections.

Quickstart:
  1. muxcat emqx retained ls
  2. muxcat emqx retained show sensors/cfg
  3. muxcat emqx retained rm sensors/cfg`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newRetainedLsCmd(),
		newRetainedShowCmd(),
		newRetainedRmCmd(),
	)
	return c
}

func newRetainedLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List topics holding a retained message (paginated)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx retained ls
  muxcat emqx retained ls --limit 50 --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			items, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/mqtt/retainer/messages", nil, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, []any{
					strOf(it, "topic"),
					strOf(it, "msgid"),
					strOf(it, "from_clientid"),
					strOf(it, "from_username"),
					strOf(it, "publish_at"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"topic", "msgid", "from_clientid", "from_username", "publish_at"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
}

func newRetainedShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <topic>",
		Short: "Show a retained message (payload truncated to 4KB)",
		Args:  cli.ExactArgs(1, "<topic>", "topic"),
		Example: `  muxcat emqx retained show sensors/cfg
  muxcat emqx retained show sensors/cfg --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			topic := args[0]
			r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/mqtt/retainer/message/"+url.PathEscape(topic), nil, nil, false)
			if err != nil {
				return err
			}
			var v map[string]any
			if err := decodeJSON(r.body, &v); err != nil {
				return err
			}
			// EMQX returns the payload base64-encoded; decode it for
			// display, keeping the base64 form (with a note) when it is
			// not valid UTF-8 text. Truncation applies to the decoded
			// form.
			if p := strOf(v, "payload"); p != "" {
				if raw, err := base64.StdEncoding.DecodeString(p); err == nil && utf8.Valid(raw) {
					s := string(raw)
					if len(s) > retainedPayloadMax {
						s = s[:retainedPayloadMax]
						v["payload_truncated"] = true
						v["note"] = "payload truncated to 4KB"
					}
					v["payload"] = s
				} else {
					if len(p) > retainedPayloadMax {
						p = p[:retainedPayloadMax]
						v["payload_truncated"] = true
					}
					v["payload"] = p
					v["note"] = "payload is not UTF-8 text, shown base64-encoded"
				}
			}
			return cli.RenderResult(cmd, &output.Result{Value: v, Syntax: "yaml"}, meta(name, start, false))
		},
	}
}

func newRetainedRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <topic>",
		Short:   "Delete a retained message; blocked on readonly connections",
		Args:    cli.ExactArgs(1, "<topic>", "topic"),
		Example: `  muxcat emqx retained rm sensors/cfg`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "retained rm"); err != nil {
				return err
			}
			topic := args[0]
			if _, err := cl.send(cmd.Context(), http.MethodDelete, "/api/v5/mqtt/retainer/message/"+url.PathEscape(topic), nil, nil, false); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  "deleted retained message on " + topic,
				JSONData: map[string]any{"topic": topic, "deleted": true},
			}, meta(name, start, false))
		},
	}
}
