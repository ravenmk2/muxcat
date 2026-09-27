package nacos

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newNamespaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "namespace",
		Aliases: []string{"ns"},
		Short:   "Manage Nacos namespaces",
		Long: `Manage Nacos namespaces (tenants). namespace ls lists each
namespace with its show name, quota and config count; create/update
manage namespaces and are blocked on readonly connections. Switch
the working namespace of a data command with --namespace (alias
--ns), or set a default on the connection (conn add --namespace).

Quickstart:
  1. muxcat nacos namespace create staging --desc "pre-release"
  2. muxcat nacos namespace ls
  3. muxcat nacos config ls --ns staging`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newNamespaceLsCmd(),
		newNamespaceCreateCmd(),
		newNamespaceUpdateCmd(),
	)
	return c
}

func newNamespaceLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List namespaces",
		Args:  cobra.NoArgs,
		Example: `  muxcat nacos namespace ls
  muxcat nacos ns ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			nss, err := cl.api().namespaceList(cmd.Context())
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(nss))
			for _, ns := range nss {
				rows = append(rows, []any{ns.Namespace, ns.ShowName, ns.Quota, ns.ConfigCount})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"namespace", "showName", "quota", "configCount"},
				Rows:    rows,
			}, meta(name, start, false))
		},
	}
}

func newNamespaceCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create <namespaceId>",
		Short: "Create a namespace",
		Long: `Create a namespace. The namespace id is the address key used
by --namespace on data commands; --name sets the display name and
defaults to the id. Blocked on readonly connections.`,
		Args: cli.ExactArgs(1, "<namespaceId>", "namespaceId"),
		Example: `  muxcat nacos namespace create staging
  muxcat nacos ns create staging --name "Staging Env" --desc "pre-release"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "namespace create"); err != nil {
				return err
			}
			id := args[0]
			showName := cli.FlagString(cmd, "name")
			if showName == "" {
				showName = id
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			if err := cl.api().namespaceCreate(cmd.Context(), id, showName, cli.FlagString(cmd, "desc")); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("created namespace %s (%s)", id, showName),
				JSONData: map[string]any{"namespace": id, "showName": showName, "created": true},
			}, meta(name, start, false))
		},
	}
	c.Flags().String("name", "", "display name of the namespace (default: the namespace id)")
	c.Flags().String("desc", "", "namespace description")
	return c
}

func newNamespaceUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <namespaceId>",
		Short: "Update a namespace's display name or description",
		Long: `Update a namespace. The server requires the display name on
every update, so a missing --name reuses the namespace's current
one. Blocked on readonly connections.`,
		Args: cli.ExactArgs(1, "<namespaceId>", "namespaceId"),
		Example: `  muxcat nacos namespace update staging --name "Staging (frozen)"
  muxcat nacos ns update staging --desc "retired after v2"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "namespace update"); err != nil {
				return err
			}
			id := args[0]
			showName := cli.FlagString(cmd, "name")
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			if showName == "" {
				nss, err := cl.api().namespaceList(cmd.Context())
				if err != nil {
					return err
				}
				for _, ns := range nss {
					if ns.Namespace == id {
						showName = ns.ShowName
						break
					}
				}
				if showName == "" {
					return output.NewError(output.CodeQueryError,
						"namespace not found: "+id, "list namespaces with muxcat nacos namespace ls")
				}
			}
			if err := cl.api().namespaceUpdate(cmd.Context(), id, showName, cli.FlagString(cmd, "desc")); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("updated namespace %s (%s)", id, showName),
				JSONData: map[string]any{"namespace": id, "showName": showName, "updated": true},
			}, meta(name, start, false))
		},
	}
	c.Flags().String("name", "", "new display name (default: keep the current one)")
	c.Flags().String("desc", "", "new namespace description")
	return c
}
