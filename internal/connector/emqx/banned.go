package emqx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newBannedCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "banned",
		Short: "Manage the banned (blacklist) list",
		Long: `Manage the banned list (blacklist of clientids, usernames or
peer hosts). banned ls lists entries (paginated); banned add/rm modify
the list and are blocked on readonly connections.

Quickstart:
  1. muxcat emqx banned ls
  2. muxcat emqx banned add bad-client --as clientid --reason spam
  3. muxcat emqx banned rm bad-client --as clientid`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newBannedLsCmd(),
		newBannedAddCmd(),
		newBannedRmCmd(),
	)
	return c
}

// bannedAs validates the --as flag value (the banned object kind).
func bannedAs(cmd *cobra.Command) (string, error) {
	as := cli.FlagString(cmd, "as")
	switch as {
	case "clientid", "username", "peerhost":
		return as, nil
	default:
		return "", output.NewError(output.CodeMissingArgument,
			"invalid --as value: "+as, "valid values: clientid|username|peerhost")
	}
}

func newBannedLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List banned entries (paginated)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx banned ls
  muxcat emqx banned ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			items, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/banned", nil, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, []any{
					strOf(it, "as"),
					strOf(it, "who"),
					strOf(it, "by"),
					strOf(it, "reason"),
					strOf(it, "at"),
					strOf(it, "until"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"as", "who", "by", "reason", "at", "until"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
}

func newBannedAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <who>",
		Short: "Ban a clientid, username or peer host; blocked on readonly connections",
		Args:  cli.ExactArgs(1, "<who>", "who"),
		Example: `  muxcat emqx banned add bad-client --as clientid --reason "spam"
  muxcat emqx banned add 10.0.0.7 --as peerhost --until 1893456000`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "banned add"); err != nil {
				return err
			}
			as, err := bannedAs(cmd)
			if err != nil {
				return err
			}
			entry := map[string]any{"as": as, "who": args[0]}
			if v := cli.FlagString(cmd, "reason"); v != "" {
				entry["reason"] = v
			}
			if v := cli.FlagString(cmd, "until"); v != "" {
				// EMQX expects a unix timestamp (seconds).
				ts, err := strconv.ParseInt(v, 10, 64)
				if err != nil {
					return output.NewError(output.CodeMissingArgument,
						"invalid --until value: "+v, "a unix timestamp in seconds, e.g. 1893456000")
				}
				entry["until"] = ts
			}
			body, _ := json.Marshal(entry)
			if _, err := cl.send(cmd.Context(), http.MethodPost, "/api/v5/banned", nil, body, false); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("banned %s %s", as, args[0]),
				JSONData: map[string]any{"as": as, "who": args[0], "banned": true},
			}, meta(name, start, false))
		},
	}
	c.Flags().String("as", "", "banned object kind: clientid|username|peerhost (required)")
	c.Flags().String("reason", "", "ban reason")
	c.Flags().String("until", "", "ban expiry as a unix timestamp in seconds (default: never)")
	return c
}

func newBannedRmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rm <who>",
		Short: "Remove a banned entry; blocked on readonly connections",
		Args:  cli.ExactArgs(1, "<who>", "who"),
		Example: `  muxcat emqx banned rm bad-client --as clientid
  muxcat emqx banned rm 10.0.0.7 --as peerhost`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "banned rm"); err != nil {
				return err
			}
			as, err := bannedAs(cmd)
			if err != nil {
				return err
			}
			who := args[0]
			path := "/api/v5/banned/" + url.PathEscape(as) + "/" + url.PathEscape(who)
			if _, err := cl.send(cmd.Context(), http.MethodDelete, path, nil, nil, false); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("unbanned %s %s", as, who),
				JSONData: map[string]any{"as": as, "who": who, "banned": false},
			}, meta(name, start, false))
		},
	}
	c.Flags().String("as", "", "banned object kind: clientid|username|peerhost (required)")
	return c
}
