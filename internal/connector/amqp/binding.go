package amqp

import (
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
		Long: `Manage bindings: create one (bind) or remove one (unbind),
addressed by --exchange, --queue and --routing-key.

AMQP 1.0 identifies a binding by a server-generated path (returned
by bind in binding_path); unbind reconstructs it from the
(exchange, queue, routing-key) triple, which covers bindings
without arguments. Neither protocol has a list primitive, so there
is no binding ls — use rmq binding ls (Management API).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newBindingBindCmd(), newBindingUnbindCmd())
	return c
}

func newBindingBindCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "bind",
		Short: "Create a binding from an exchange to a queue",
		Args:  cobra.NoArgs,
		Example: `  muxcat amqp binding bind --exchange events --queue my-queue
  muxcat amqp binding bind --exchange events --queue my-queue --routing-key 'app.*'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "binding bind")
			if err != nil {
				return err
			}
			defer env.close()
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
			routingKey := cli.FlagString(cmd, "routing-key")
			path, err := env.session.Bind(env.ctx, exchange, queue, routingKey, arguments)
			if err != nil {
				return err
			}
			value := map[string]any{
				"exchange": exchange, "queue": queue,
				"routing_key": routingKey, "bound": true,
			}
			if path != "" {
				value["binding_path"] = path
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   value,
				Message: fmt.Sprintf("bound %s -> %s in vhost %s", exchange, queue, env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().String("exchange", "", "source exchange (required)")
	c.Flags().String("queue", "", "destination queue (required)")
	c.Flags().String("routing-key", "", "binding key")
	c.Flags().String("args", "", "binding arguments as a JSON object")
	return c
}

func newBindingUnbindCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unbind",
		Short: "Remove a binding (matched by exchange, queue and routing key)",
		Args:  cobra.NoArgs,
		Example: `  muxcat amqp binding unbind --exchange events --queue my-queue
  muxcat amqp binding unbind --exchange events --queue my-queue --routing-key 'app.*'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			env, err := openForCmd(cmd, "binding unbind")
			if err != nil {
				return err
			}
			defer env.close()
			exchange := cli.FlagString(cmd, "exchange")
			queue := cli.FlagString(cmd, "queue")
			if exchange == "" || queue == "" {
				return output.NewError(output.CodeMissingArgument,
					"binding unbind requires --exchange and --queue", "both sides of the binding are required")
			}
			routingKey := cli.FlagString(cmd, "routing-key")
			if err := env.session.Unbind(env.ctx, exchange, queue, routingKey); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{
					"exchange": exchange, "queue": queue,
					"routing_key": routingKey, "unbound": true,
				},
				Message: fmt.Sprintf("unbound %s -> %s in vhost %s", exchange, queue, env.vhost),
			}, meta(env.name, start, false))
		},
	}
	addVhostFlag(c)
	c.Flags().String("exchange", "", "source exchange (required)")
	c.Flags().String("queue", "", "destination queue (required)")
	c.Flags().String("routing-key", "", "binding key")
	return c
}
