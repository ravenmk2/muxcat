package rabbitmq

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newBindingCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "binding",
		Short: "Manage bindings between exchanges and queues",
		Long: `Manage bindings: list them (optionally filtered by vhost,
exchange and queue), create one, or remove one.

Removing a binding needs its properties key: take the props value from
binding ls output and pass it as --props to binding unbind.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newBindingLsCmd(), newBindingBindCmd(), newBindingUnbindCmd())
	return c
}

// bindingListPath resolves the listing endpoint from the filter flags:
// /api/bindings, /api/bindings/<v>, /api/bindings/<v>/e/<e>, or
// /api/bindings/<v>/e/<e>/q/<q>.
func bindingListPath(vhost, exchange, queue string, vhostSet bool) string {
	switch {
	case exchange != "" && queue != "":
		return fmt.Sprintf("/api/bindings/%s/e/%s/q/%s", esc(vhost), esc(exchange), esc(queue))
	case exchange != "":
		return fmt.Sprintf("/api/bindings/%s/e/%s", esc(vhost), esc(exchange))
	case queue != "":
		return fmt.Sprintf("/api/bindings/%s/q/%s", esc(vhost), esc(queue))
	case vhostSet:
		return "/api/bindings/" + esc(vhost)
	}
	return "/api/bindings"
}

func newBindingLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List bindings (GET /api/bindings)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq binding ls
  muxcat rabbitmq binding ls --vhost /
  muxcat rabbitmq binding ls --exchange amq.direct --queue my-queue`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := bindingListPath(vhostFlag(cmd), cli.FlagString(cmd, "exchange"),
				cli.FlagString(cmd, "queue"), cmd.Flags().Changed("vhost"))
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
				b := obj(item)
				rows = append(rows, []any{
					str(b["vhost"]), str(b["source"]), str(b["destination"]),
					str(b["destination_type"]), str(b["routing_key"]),
					str(b["properties_key"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"vhost", "source", "destination", "type", "routing_key", "props"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	c.Flags().String("exchange", "", "restrict the listing to bindings from this exchange")
	c.Flags().String("queue", "", "restrict the listing to bindings to this queue")
	return c
}

func newBindingBindCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "bind",
		Short: "Create a binding (POST /api/bindings/<vhost>/e/<exchange>/q/<queue>)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq binding bind --exchange events --queue my-queue
  muxcat rabbitmq binding bind --exchange events --queue my-queue --routing-key 'app.*'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "binding bind"); err != nil {
				return err
			}
			exchange := cli.FlagString(cmd, "exchange")
			queue := cli.FlagString(cmd, "queue")
			if exchange == "" || queue == "" {
				return output.NewError(output.CodeMissingArgument,
					"binding bind requires --exchange and --queue", "both sides of the binding are required")
			}
			arguments, err := parseJSONArg("--args", cli.FlagString(cmd, "args"))
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{
				"routing_key": cli.FlagString(cmd, "routing-key"),
				"arguments":   arguments,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/bindings/%s/e/%s/q/%s",
				esc(vhostFlag(cmd)), esc(exchange), esc(queue))
			if _, err := cl.do(cmd.Context(), "POST", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{
					"vhost": vhostFlag(cmd), "exchange": exchange, "queue": queue,
					"routing_key": cli.FlagString(cmd, "routing-key"), "bound": true,
				},
				Message: fmt.Sprintf("bound %s -> %s in vhost %s", exchange, queue, vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the binding (default: /)")
	c.Flags().String("exchange", "", "source exchange (required)")
	c.Flags().String("queue", "", "destination queue (required)")
	c.Flags().String("routing-key", "", "binding key")
	c.Flags().String("args", "", "binding arguments as a JSON object")
	return c
}

func newBindingUnbindCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unbind",
		Short: "Remove a binding (DELETE /api/bindings/<vhost>/e/<exchange>/q/<queue>/<props>)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq binding unbind --exchange events --queue my-queue --props 'app.*'
  muxcat rabbitmq binding unbind --exchange events --queue my-queue --props '~'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "binding unbind"); err != nil {
				return err
			}
			exchange := cli.FlagString(cmd, "exchange")
			queue := cli.FlagString(cmd, "queue")
			props := cli.FlagString(cmd, "props")
			if exchange == "" || queue == "" || props == "" {
				return output.NewError(output.CodeMissingArgument,
					"binding unbind requires --exchange, --queue and --props",
					"props is the properties_key shown by binding ls (an empty routing key shows as ~)")
			}
			path := fmt.Sprintf("/api/bindings/%s/e/%s/q/%s/%s",
				esc(vhostFlag(cmd)), esc(exchange), esc(queue), esc(props))
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{
					"vhost": vhostFlag(cmd), "exchange": exchange, "queue": queue, "unbound": true,
				},
				Message: fmt.Sprintf("unbound %s -> %s in vhost %s", exchange, queue, vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the binding (default: /)")
	c.Flags().String("exchange", "", "source exchange (required)")
	c.Flags().String("queue", "", "destination queue (required)")
	c.Flags().String("props", "", "properties key of the binding, from binding ls output (required)")
	return c
}
