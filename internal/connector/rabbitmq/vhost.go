package rabbitmq

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newVhostCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "vhost",
		Short: "Manage virtual hosts",
		Long: `Manage virtual hosts: list them with message totals, show one
vhost's detail, add one, or remove one. The default vhost "/" is
encoded as %2F in API paths.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newVhostLsCmd(), newVhostShowCmd(), newVhostAddCmd(), newVhostDeleteCmd())
	return c
}

func newVhostLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List virtual hosts (GET /api/vhosts)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq vhost ls
  muxcat rabbitmq vhost ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/vhosts", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				v := obj(item)
				var tags []string
				if ts, ok := v["tags"].([]any); ok {
					for _, t := range ts {
						tags = append(tags, str(t))
					}
				}
				rows = append(rows, []any{
					str(v["name"]), boolOf(v["tracing"]),
					numOf(v["messages"]), numOf(v["messages_ready"]),
					numOf(v["messages_unacknowledged"]),
					str(v["description"]), strings.Join(tags, ","),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "tracing", "messages", "ready", "unacked", "description", "tags"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newVhostShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a vhost's full detail (GET /api/vhosts/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq vhost show /
  muxcat rabbitmq vhost show / --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/vhosts/"+esc(args[0]), nil)
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

func newVhostAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a virtual host (PUT /api/vhosts/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq vhost add staging
  muxcat rabbitmq vhost add staging --description "staging env" --tags team-a,team-b`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "vhost add"); err != nil {
				return err
			}
			var tags []string
			for _, t := range strings.Split(cli.FlagString(cmd, "tags"), ",") {
				if t = strings.TrimSpace(t); t != "" {
					tags = append(tags, t)
				}
			}
			body, err := jsonMarshalVhost(cli.FlagString(cmd, "description"), tags)
			if err != nil {
				return err
			}
			if _, err := cl.do(cmd.Context(), "PUT", "/api/vhosts/"+esc(args[0]), body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"vhost": args[0], "created": true},
				Message: "added vhost " + args[0],
			}, meta(name, start, false))
		},
	}
	c.Flags().String("description", "", "vhost description (RabbitMQ >= 3.13)")
	c.Flags().String("tags", "", "comma-separated vhost tags (RabbitMQ >= 3.13)")
	return c
}

// jsonMarshalVhost builds the PUT body; description/tags are only sent
// when set, so pre-3.13 servers are not confused by unknown fields.
func jsonMarshalVhost(description string, tags []string) ([]byte, error) {
	m := map[string]any{}
	if description != "" {
		m["description"] = description
	}
	if len(tags) > 0 {
		m["tags"] = tags
	}
	return json.Marshal(m)
}

func newVhostDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete a virtual host (DELETE /api/vhosts/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq vhost delete staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "vhost delete"); err != nil {
				return err
			}
			if _, err := cl.do(cmd.Context(), "DELETE", "/api/vhosts/"+esc(args[0]), nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"vhost": args[0], "deleted": true},
				Message: "deleted vhost " + args[0],
			}, meta(name, start, false))
		},
	}
}
