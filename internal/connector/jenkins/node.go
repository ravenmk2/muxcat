package jenkins

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newNodeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "node",
		Short: "Inspect Jenkins nodes (agents)",
		Long: `Inspect Jenkins nodes: list the controller and agents, their
online status, and how busy their executors are.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newNodeLsCmd())
	return c
}

func newNodeLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List nodes (GET /computer/api/json)",
		Args:  cobra.NoArgs,
		Example: `  muxcat jenkins node ls
  muxcat jenkins node ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET",
				"/computer/api/json?tree=computer[displayName,offline,temporarilyOffline,idle,numExecutors,executors[currentExecutable[fullName,number]],description]", nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			var rows [][]any
			if m, ok := raw.(map[string]any); ok {
				if computers, ok := m["computer"].([]any); ok {
					for _, item := range computers {
						n, ok := item.(map[string]any)
						if !ok {
							continue
						}
						rows = append(rows, []any{
							str(n["displayName"]), nodeStatus(n),
							boolOf(n["idle"]), executors(n), str(n["description"]),
						})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "status", "idle", "executors", "description"},
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
}

// nodeStatus derives the one-word status of a node; a temporarily offline
// node is distinguished from a hard-offline one.
func nodeStatus(n map[string]any) string {
	switch {
	case boolOf(n["temporarilyOffline"]):
		return "temp-offline"
	case boolOf(n["offline"]):
		return "offline"
	}
	return "online"
}

// executors renders executor usage as "busy/total".
func executors(n map[string]any) string {
	total, _ := n["numExecutors"].(float64)
	busy := 0
	if exs, ok := n["executors"].([]any); ok {
		for _, e := range exs {
			if em, ok := e.(map[string]any); ok {
				if ce, ok := em["currentExecutable"].(map[string]any); ok && ce != nil {
					busy++
				}
			}
		}
	}
	return fmt.Sprintf("%d/%d", busy, int(total))
}
