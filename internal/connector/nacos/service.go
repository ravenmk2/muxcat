package nacos

import (
	"encoding/json"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newServiceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "Browse Nacos services",
		Long: `Browse Nacos services (naming/discovery registry). ls lists
the services of the namespace, show details one service. Services
are addressed by name plus group (-g, default DEFAULT_GROUP) and
namespace (--namespace, alias --ns, default public).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newServiceLsCmd(),
		newServiceShowCmd(),
	)
	return c
}

func newServiceLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List services in the namespace",
		Args:  cobra.NoArgs,
		Example: `  muxcat nacos service ls
  muxcat nacos service ls --limit 20 --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			namespace := namespaceOf(cmd, conn)
			limit := cli.FlagLimit(cmd)
			pageSize := limit
			if pageSize <= 0 || pageSize > 500 {
				pageSize = 500
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			page, err := cl.api().serviceList(cmd.Context(), namespace, 1, pageSize)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(page.Services))
			for _, s := range page.Services {
				rows = append(rows, []any{s.Name, s.Group})
			}
			rows, truncated := applyLimit(rows, limit)
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"service", "group"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	addNamespaceFlag(c)
	return c
}

func newServiceShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show <serviceName>",
		Short: "Show a service's detail",
		Args:  cli.ExactArgs(1, "<serviceName>", "serviceName"),
		Example: `  muxcat nacos service show order-service
  muxcat nacos service show order-service -g BIZ_GROUP --namespace staging --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			detail, err := cl.api().serviceDetail(cmd.Context(), args[0], groupOf(cmd), namespaceOf(cmd, conn))
			if err != nil {
				return err
			}
			res := &output.Result{JSONData: detail}
			if b, err := json.MarshalIndent(detail, "", "  "); err == nil {
				res.Value = string(b)
				res.Syntax = "json"
			}
			return cli.RenderResult(cmd, res, meta(name, start, false))
		},
	}
	c.Flags().StringP("group", "g", "", "service group (default DEFAULT_GROUP)")
	addNamespaceFlag(c)
	return c
}

func newInstanceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "instance",
		Short: "Browse Nacos service instances",
		Long: `Browse the instances registered under a Nacos service.
instance ls lists ip/port/weight/healthy/enabled per instance of a
service, addressed by name plus group (-g, default DEFAULT_GROUP)
and namespace (--namespace, alias --ns, default public).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newInstanceLsCmd())
	return c
}

func newInstanceLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls <serviceName>",
		Short: "List the instances of a service",
		Args:  cli.ExactArgs(1, "<serviceName>", "serviceName"),
		Example: `  muxcat nacos instance ls order-service
  muxcat nacos instance ls order-service -g BIZ_GROUP --namespace staging --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			hosts, err := cl.api().instanceList(cmd.Context(), args[0], groupOf(cmd), namespaceOf(cmd, conn))
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(hosts))
			for _, h := range hosts {
				rows = append(rows, []any{h.IP, h.Port, h.Weight, h.Healthy, h.Enabled})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"ip", "port", "weight", "healthy", "enabled"},
				Rows:    rows,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().StringP("group", "g", "", "service group (default DEFAULT_GROUP)")
	addNamespaceFlag(c)
	return c
}
