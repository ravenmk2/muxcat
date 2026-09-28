package rabbitmq

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newUserCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "user",
		Short: "Manage broker users",
		Long: `Manage broker users: list them, show one, add one (with a
password and tags), change a password, or delete one. Passwords are
never echoed in any output; grant access to vhosts with the
permission group.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newUserLsCmd(),
		newUserShowCmd(),
		newUserAddCmd(),
		newUserPasswdCmd(),
		newUserDeleteCmd(),
	)
	return c
}

// userTags renders the tags field across versions: a comma-separated
// string on older servers, a list on newer ones.
func userTags(v any) string {
	if s := str(v); s != "" {
		return s
	}
	if ts, ok := v.([]any); ok {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			out = append(out, str(t))
		}
		return strings.Join(out, ",")
	}
	return ""
}

func newUserLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List users (GET /api/users)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq user ls
  muxcat rabbitmq user ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/users", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				u := obj(item)
				rows = append(rows, []any{
					str(u["name"]), userTags(u["tags"]), boolOf(u["is_internal"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "tags", "is_internal"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newUserShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a user's detail (GET /api/users/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq user show admin
  muxcat rabbitmq user show admin --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/users/"+esc(args[0]), nil)
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

// resolveUserPassword resolves the password for user add/passwd: the
// --password flag (plaintext, warned on stderr) wins; otherwise an
// interactive session prompts for it. required=true (user passwd)
// additionally rejects an empty password in both modes: a PUT without a
// password would silently clear it server-side.
// The returned password lives in memory only and is never echoed.
func resolveUserPassword(cmd *cobra.Command, required bool) (string, error) {
	var password string
	switch {
	case cmd.Flags().Changed("password"):
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"Warning: --password passes the credential in plaintext; prefer an interactive prompt")
		password = cli.FlagString(cmd, "password")
	case cli.RuntimeFrom(cmd.Context()).Interactive:
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("Password (empty for no password)").
				EchoMode(huh.EchoModePassword).
				Value(&password),
		))
		if err := form.Run(); err != nil {
			return "", err
		}
	case required:
		return "", output.NewError(output.CodeMissingArgument,
			"missing required flag --password in non-interactive environments",
			"pass --password, or run on a TTY for the password prompt")
	}
	if required && password == "" {
		return "", output.NewError(output.CodeConfigInvalid,
			"empty password is not allowed for user passwd",
			"a password change requires a non-empty password; to make a login-less user, delete it and re-add it with user add (no --password)")
	}
	return password, nil
}

// putUser sends the create/update body for a user. The plaintext password
// never appears in any output. An empty password omits the field — for an
// existing user the server then takes its clear_password branch and
// silently clears the password, so callers must guarantee this never
// happens: user add refuses existing users up front, user passwd always
// sends a non-empty password (only a genuinely new, intentionally
// login-less user is created without one). tags is always sent (possibly
// empty): RabbitMQ rejects a PUT without the tags field
// (tags_not_present) — this predates 4.x, present in 3.13 and earlier.
func putUser(cmd *cobra.Command, cl *client, user, password, tags string) error {
	m := map[string]any{"tags": tags}
	if password != "" {
		m["password"] = password
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = cl.do(cmd.Context(), "PUT", "/api/users/"+esc(user), body)
	return err
}

// userExists probes GET /api/users/<name>: 404 means the name is free.
func userExists(cmd *cobra.Command, cl *client, user string) (bool, error) {
	path := "/api/users/" + esc(user)
	resp, err := cl.exchange(cmd.Context(), "GET", path, nil, "")
	if err != nil {
		return false, err
	}
	if resp.status == http.StatusNotFound {
		return false, nil
	}
	if resp.status < 200 || resp.status >= 300 {
		return false, classifyStatus(resp.status, resp.body, path)
	}
	return true, nil
}

// currentUserTags reads a user's existing tags (as a comma string), so a
// password change does not wipe them.
func currentUserTags(cmd *cobra.Command, cl *client, user string) (string, error) {
	resp, err := cl.do(cmd.Context(), "GET", "/api/users/"+esc(user), nil)
	if err != nil {
		return "", err
	}
	raw, err := decodeBody(resp.body)
	if err != nil {
		return "", err
	}
	return userTags(obj(raw)["tags"]), nil
}

func newUserAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a user (PUT /api/users/<name>)",
		Long: `Create a user. add is create-only: if the user already exists
the command fails instead of updating, because a PUT without a
password would silently clear the existing one. Use user passwd to
change a password, user delete to remove the user first.`,
		Args: cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq user add alice --tags administrator
  muxcat rabbitmq user add bob --password s3cr3t --tags management
  muxcat rabbitmq user add carol`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "user add"); err != nil {
				return err
			}
			// Refuse to upsert: a PUT without a password clears the
			// existing password server-side (clear_password branch).
			exists, err := userExists(cmd, cl, args[0])
			if err != nil {
				return err
			}
			if exists {
				return output.NewError(output.CodeConfigInvalid,
					"user already exists: "+args[0],
					"change its password with user passwd, or remove it first with user delete")
			}
			password, err := resolveUserPassword(cmd, false)
			if err != nil {
				return err
			}
			tags := strings.Join(splitTags(cli.FlagString(cmd, "tags")), ",")
			if err := putUser(cmd, cl, args[0], password, tags); err != nil {
				return err
			}
			message := "added user " + args[0]
			if password == "" {
				message += " (no password set; the user cannot log in yet)"
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"user": args[0], "tags": tags, "created": true},
				Message: message,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("password", "", "user password (plaintext flag; prefer the interactive prompt)")
	c.Flags().String("tags", "", "comma-separated user tags (e.g. administrator,management)")
	return c
}

func newUserPasswdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "passwd <name>",
		Short: "Change a user's password (PUT /api/users/<name>)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq user passwd alice
  muxcat rabbitmq user passwd alice --password n3w-s3cr3t`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "user passwd"); err != nil {
				return err
			}
			password, err := resolveUserPassword(cmd, true)
			if err != nil {
				return err
			}
			// The PUT requires the tags field; keep the user's existing
			// tags so the password change does not strip roles.
			tags, err := currentUserTags(cmd, cl, args[0])
			if err != nil {
				return err
			}
			if err := putUser(cmd, cl, args[0], password, tags); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"user": args[0], "password_changed": true},
				Message: "changed password of user " + args[0],
			}, meta(name, start, false))
		},
	}
	c.Flags().String("password", "", "new password (plaintext flag; prefer the interactive prompt)")
	return c
}

func newUserDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"del", "rm"},
		Short:   "Delete a user (DELETE /api/users/<name>)",
		Args:    cli.ExactArgs(1, "<name>", "name"),
		Example: `  muxcat rabbitmq user delete bob
  muxcat rabbitmq user rm bob`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "user delete"); err != nil {
				return err
			}
			if _, err := cl.do(cmd.Context(), "DELETE", "/api/users/"+esc(args[0]), nil); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"user": args[0], "deleted": true},
				Message: "deleted user " + args[0],
			}, meta(name, start, false))
		},
	}
}

// splitTags parses a comma-separated tag list.
func splitTags(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
