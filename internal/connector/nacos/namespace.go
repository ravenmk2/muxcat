package nacos

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newNamespaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "namespace",
		Short: "Browse Nacos namespaces",
		Long: `Browse Nacos namespaces (tenants). namespace ls lists each
namespace with its show name, quota and config count. Switch the
working namespace of a data command with --namespace (alias --ns),
or set a default on the connection (conn add --namespace).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newNamespaceLsCmd())
	return c
}

func newNamespaceLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List namespaces",
		Args:  cobra.NoArgs,
		Example: `  muxcat nacos namespace ls
  muxcat nacos namespace ls --json`,
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
