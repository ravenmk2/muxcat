package rabbitmq

import (
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newOverviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "overview",
		Short: "Broker overview (GET /api/overview)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq overview
  muxcat rabbitmq overview --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/overview", nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			rows := overviewRows(obj(raw))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"metric", "value"},
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, false))
		},
	}
}

// overviewRows flattens the overview document into a metric/value summary:
// identity fields first, then object and queue totals. Fields missing from
// older servers (e.g. 3.8) simply yield no row.
func overviewRows(m map[string]any) [][]any {
	var rows [][]any
	add := func(k string, v any) {
		if s := str(v); s != "" {
			rows = append(rows, []any{k, s})
		}
	}
	add("product", m["product_name"])
	add("version", m["rabbitmq_version"])
	add("cluster", m["cluster_name"])
	add("erlang", m["erlang_version"])
	for _, section := range []string{"object_totals", "queue_totals"} {
		totals := obj(m[section])
		keys := make([]string, 0, len(totals))
		for k := range totals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if f, ok := totals[k].(float64); ok {
				rows = append(rows, []any{k, int64(f)})
			}
		}
	}
	return rows
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated identity (GET /api/whoami)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq whoami
  muxcat rabbitmq whoami --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/whoami", nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			m := obj(raw)
			return cli.RenderResult(cmd, &output.Result{
				Value: map[string]any{
					"name": str(m["name"]),
					"tags": userTags(m["tags"]),
				},
				JSONData: raw,
				Syntax:   "yaml",
			}, meta(name, start, false))
		},
	}
}
