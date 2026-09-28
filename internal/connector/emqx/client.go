package emqx

import (
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newClientCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "client",
		Short: "Inspect and kick connected MQTT clients",
		Long: `Inspect connected MQTT clients. client ls lists clients
(paginated, server-side filters pass through); client show prints one
client's full detail; client kick disconnects a client and is blocked
on readonly connections.

Quickstart:
  1. muxcat emqx client ls
  2. muxcat emqx client show my-client-id
  3. muxcat emqx client kick my-client-id`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newClientLsCmd(),
		newClientShowCmd(),
		newClientKickCmd(),
	)
	return c
}

func newClientLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List connected clients (paginated; filters pass through to the server)",
		Args:  cobra.NoArgs,
		Example: `  muxcat emqx client ls
  muxcat emqx client ls --username sensor --limit 20
  muxcat emqx client ls --like-clientid dev- --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			params := url.Values{}
			// flag name -> server query parameter
			for flag, param := range map[string]string{
				"clientid":      "clientid",
				"username":      "username",
				"like-clientid": "like_clientid",
				"like-username": "like_username",
			} {
				if v := cli.FlagString(cmd, flag); v != "" {
					params.Set(param, v)
				}
			}
			items, truncated, err := cl.fetchPages(cmd.Context(), "/api/v5/clients", params, cli.FlagLimit(cmd))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(items))
			for _, it := range items {
				rows = append(rows, []any{
					strOf(it, "clientid"),
					strOf(it, "username"),
					strOf(it, "node"),
					strOf(it, "ip_address"),
					intOf(it, "port"),
					it["connected"],
					strOf(it, "connected_at"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"clientid", "username", "node", "ip_address", "port", "connected", "connected_at"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("clientid", "", "filter by exact clientid")
	c.Flags().String("username", "", "filter by exact username")
	c.Flags().String("like-clientid", "", "filter by clientid substring")
	c.Flags().String("like-username", "", "filter by username substring")
	return c
}

func newClientShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <clientid>",
		Short: "Show one client's full detail",
		Args:  cli.ExactArgs(1, "<clientid>", "clientid"),
		Example: `  muxcat emqx client show my-client-id
  muxcat emqx client show "dev/#1" --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			id := args[0]
			r, err := cl.send(cmd.Context(), http.MethodGet, "/api/v5/clients/"+url.PathEscape(id), nil, nil, false)
			if err != nil {
				return err
			}
			// The API answers an array of one entry.
			var arr []map[string]any
			if err := decodeJSON(r.body, &arr); err != nil {
				return err
			}
			if len(arr) == 0 {
				return output.NewError(output.CodeQueryError,
					"client not found: "+id, "list clients with muxcat emqx client ls")
			}
			return cli.RenderResult(cmd, &output.Result{Value: arr[0], Syntax: "yaml"}, meta(name, start, false))
		},
	}
}

func newClientKickCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kick <clientid>",
		Short: "Kick (disconnect) a client; blocked on readonly connections",
		Args:  cli.ExactArgs(1, "<clientid>", "clientid"),
		Example: `  muxcat emqx client kick my-client-id
  muxcat emqx client kick "dev/#1"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "client kick"); err != nil {
				return err
			}
			id := args[0]
			if _, err := cl.send(cmd.Context(), http.MethodDelete, "/api/v5/clients/"+url.PathEscape(id), nil, nil, false); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  "kicked client " + id,
				JSONData: map[string]any{"clientid": id, "kicked": true},
			}, meta(name, start, false))
		},
	}
}
