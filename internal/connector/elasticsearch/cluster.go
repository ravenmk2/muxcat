package elasticsearch

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newClusterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cluster",
		Short: "Inspect the Elasticsearch cluster",
		Long: `Inspect cluster state: health (status, nodes, shards) or the
node list with roles and resource usage from the _cat API.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newClusterHealthCmd(), newClusterNodesCmd())
	return c
}

// clusterHealthFields are the well-known stable fields of
// GET /_cluster/health, present on ES 7.x/8.x/9.x.
var clusterHealthFields = []string{
	"status", "cluster_name", "timed_out",
	"number_of_nodes", "number_of_data_nodes",
	"active_primary_shards", "active_shards",
	"relocating_shards", "initializing_shards", "unassigned_shards",
	"delayed_unassigned_shards", "number_of_pending_tasks",
	"number_of_in_flight_fetch", "task_max_waiting_in_queue_millis",
	"active_shards_percent_as_number",
}

func newClusterHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Show cluster health (status, node counts, shard counts)",
		Args:  cobra.NoArgs,
		Example: `  muxcat es cluster health
  muxcat es cluster health --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/_cluster/health", nil)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := json.Unmarshal(resp.body, &body); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			value := make(map[string]any, len(clusterHealthFields))
			for _, f := range clusterHealthFields {
				if v, ok := body[f]; ok {
					value[f] = v
				}
			}
			message := fmt.Sprintf("cluster %s is %s (%v nodes, %v active shards)",
				value["cluster_name"], value["status"],
				value["number_of_nodes"], value["active_shards"])
			return cli.RenderResult(cmd, &output.Result{
				Value:    value,
				Message:  message,
				JSONData: body,
			}, meta(name, start, false))
		},
	}
}

func newClusterNodesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "nodes",
		Short: "List cluster nodes (role, master, version, resource usage)",
		Args:  cobra.NoArgs,
		Example: `  muxcat es cluster nodes
  muxcat es cluster nodes --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			rows, err := cl.catJSON(cmd.Context(),
				"/_cat/nodes?format=json&h=name,ip,node.role,master,version,heap.percent,ram.percent,cpu,load_1m&s=name")
			if err != nil {
				return err
			}
			out := make([][]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, []any{
					catStr(r, "name"), catStr(r, "ip"), catStr(r, "node.role"),
					catStr(r, "master"), catStr(r, "version"),
					catNum(r, "heap.percent"), catNum(r, "ram.percent"),
					catNum(r, "cpu"), catNum(r, "load_1m"),
				})
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns: []string{"name", "ip", "role", "master", "version", "heap%", "ram%", "cpu", "load_1m"},
				Rows:    out,
			}, meta(name, start, false))
		},
	}
}
