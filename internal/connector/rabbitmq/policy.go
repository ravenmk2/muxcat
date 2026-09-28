package rabbitmq

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

// policyApplyToValues enumerates the --apply-to flag values.
var policyApplyToValues = []string{"all", "queues", "exchanges", "classic_queues", "quorum_queues", "streams"}

func newPolicyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Manage policies",
		Long: `Manage policies and operator policies: list them, show one,
set one (pattern + definition), or delete one. Policies match queues
and exchanges by name pattern and inject arguments; operator policies
(--operator) are the admin-provided lower-precedence variant.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newPolicyLsCmd(),
		newPolicyShowCmd(),
		newPolicySetCmd(),
		newPolicyDeleteCmd(),
	)
	return c
}

// policyBase returns the API base for regular or operator policies.
func policyBase(cmd *cobra.Command) string {
	if cli.FlagBool(cmd, "operator") {
		return "/api/operator-policies"
	}
	return "/api/policies"
}

// policyDefinition renders a policy definition as compact JSON.
func policyDefinition(m map[string]any) string {
	b, err := json.Marshal(obj(m["definition"]))
	if err != nil {
		return ""
	}
	return string(b)
}

func newPolicyLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List policies (GET /api/policies)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq policy ls
  muxcat rabbitmq policy ls --vhost /
  muxcat rabbitmq policy ls --operator`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := policyBase(cmd)
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
				p := obj(item)
				rows = append(rows, []any{
					str(p["name"]), str(p["vhost"]), str(p["pattern"]),
					str(p["apply-to"]), numOf(p["priority"]), policyDefinition(p),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "vhost", "pattern", "apply_to", "priority", "definition"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (default: all vhosts)")
	c.Flags().Bool("operator", false, "list operator policies instead of regular policies")
	return c
}

func newPolicyShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a policy's full detail (GET /api/policies/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq policy show ha-all
  muxcat rabbitmq policy show ha-all --vhost / --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("%s/%s/%s", policyBase(cmd), esc(vhostFlag(cmd)), esc(args[0]))
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
	c.Flags().String("vhost", "", "vhost of the policy (default: /)")
	c.Flags().Bool("operator", false, "show an operator policy instead of a regular policy")
	return c
}

func newPolicySetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a policy (PUT /api/policies/<vhost>/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq policy set ttl --pattern '^temp\.' --definition '{"message-ttl":60000}' --apply-to queues
  muxcat rabbitmq policy set ha --pattern '.*' --definition '{"ha-mode":"all"}' --priority 1
  muxcat rabbitmq policy set max-len --pattern '^q' --definition '{"max-length":1000}' --operator`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "policy set"); err != nil {
				return err
			}
			pattern := cli.FlagString(cmd, "pattern")
			if pattern == "" {
				return output.NewError(output.CodeMissingArgument,
					"missing required flag --pattern", "a policy matches object names against this regex")
			}
			definition, err := parseJSONArg("--definition", cli.FlagString(cmd, "definition"))
			if err != nil {
				return err
			}
			if len(definition) == 0 {
				return output.NewError(output.CodeMissingArgument,
					"missing required flag --definition", "expected a JSON object, e.g. '{\"message-ttl\":60000}'")
			}
			applyTo := cli.FlagString(cmd, "apply-to")
			valid := false
			for _, v := range policyApplyToValues {
				if v == applyTo {
					valid = true
					break
				}
			}
			if !valid {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --apply-to: "+applyTo,
					"valid values: all|queues|exchanges|classic_queues|quorum_queues|streams")
			}
			priority, _ := cmd.Flags().GetInt("priority")
			body, err := json.Marshal(map[string]any{
				"pattern":    pattern,
				"definition": definition,
				"priority":   priority,
				"apply-to":   applyTo,
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("%s/%s/%s", policyBase(cmd), esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "PUT", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"policy": args[0], "vhost": vhostFlag(cmd), "set": true},
				Message: fmt.Sprintf("set policy %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the policy (default: /)")
	c.Flags().String("pattern", "", "name regex the policy applies to (required)")
	c.Flags().String("definition", "", "policy definition as a JSON object (required)")
	c.Flags().Int("priority", 0, "policy priority (higher wins on conflicts)")
	c.Flags().String("apply-to", "all", "what the policy applies to: all|queues|exchanges|classic_queues|quorum_queues|streams")
	c.Flags().Bool("operator", false, "set an operator policy instead of a regular policy")
	return c
}

func newPolicyDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete a policy (DELETE /api/policies/<vhost>/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq policy delete ttl
  muxcat rabbitmq policy rm ttl --vhost /`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "policy delete"); err != nil {
				return err
			}
			path := fmt.Sprintf("%s/%s/%s", policyBase(cmd), esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"policy": args[0], "vhost": vhostFlag(cmd), "deleted": true},
				Message: fmt.Sprintf("deleted policy %s in vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost of the policy (default: /)")
	c.Flags().Bool("operator", false, "delete an operator policy instead of a regular policy")
	return c
}
