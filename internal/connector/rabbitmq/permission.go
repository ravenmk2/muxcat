package rabbitmq

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newPermissionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "permission",
		Short: "Manage vhost permissions",
		Long: `Manage vhost permissions: list which users can access which
vhosts, grant a user configure/write/read access to a vhost, or
revoke it. The three scopes are regular expressions matched against
resource names.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newPermissionLsCmd(), newPermissionSetCmd(), newPermissionDeleteCmd())
	return c
}

func newPermissionLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List vhost permissions (GET /api/permissions)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq permission ls
  muxcat rabbitmq permission ls --vhost / --user alice`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/permissions", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			vhostFilter := cli.FlagString(cmd, "vhost")
			userFilter := cli.FlagString(cmd, "user")
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				p := obj(item)
				if vhostFilter != "" && str(p["vhost"]) != vhostFilter {
					continue
				}
				if userFilter != "" && str(p["user"]) != userFilter {
					continue
				}
				rows = append(rows, []any{
					str(p["user"]), str(p["vhost"]),
					str(p["configure"]), str(p["write"]), str(p["read"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"user", "vhost", "configure", "write", "read"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("vhost", "", "restrict the listing to this vhost (client-side filter)")
	c.Flags().String("user", "", "restrict the listing to this user (client-side filter)")
	return c
}

func newPermissionSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set <user>",
		Short: "Grant vhost permissions to a user (PUT /api/permissions/<vhost>/<user>)",
		Args:  cli.ExactArgs(1, "<user>", "user"),
		Example: `  muxcat rabbitmq permission set alice --configure '.*' --write '.*' --read '.*'
  muxcat rabbitmq permission set bob --vhost staging --write '^app\.' --read '^app\.'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "permission set"); err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{
				"configure": cli.FlagString(cmd, "configure"),
				"write":     cli.FlagString(cmd, "write"),
				"read":      cli.FlagString(cmd, "read"),
			})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/permissions/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "PUT", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"user": args[0], "vhost": vhostFlag(cmd), "granted": true},
				Message: fmt.Sprintf("granted %s access to vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost to grant access to (default: /)")
	c.Flags().String("configure", "", "configure-scope regex (resource names the user may declare)")
	c.Flags().String("write", "", "write-scope regex (resource names the user may publish to)")
	c.Flags().String("read", "", "read-scope regex (resource names the user may consume from)")
	return c
}

func newPermissionDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "delete <user>",
		Aliases: []string{"del", "rm"},
		Short:   "Revoke a user's vhost permissions (DELETE /api/permissions/<vhost>/<user>)",
		Args:    cli.ExactArgs(1, "<user>", "user"),
		Example: `  muxcat rabbitmq permission delete bob
  muxcat rabbitmq permission rm bob --vhost staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "permission delete"); err != nil {
				return err
			}
			path := fmt.Sprintf("/api/permissions/%s/%s", esc(vhostFlag(cmd)), esc(args[0]))
			if _, err := cl.do(cmd.Context(), "DELETE", path, nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"user": args[0], "vhost": vhostFlag(cmd), "revoked": true},
				Message: fmt.Sprintf("revoked %s access to vhost %s", args[0], vhostFlag(cmd)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "vhost to revoke access from (default: /)")
	return c
}
