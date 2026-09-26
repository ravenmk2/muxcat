package jenkins

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newQueueCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "queue",
		Short: "Inspect the Jenkins build queue",
		Long: `Inspect the Jenkins build queue: list the items waiting for
an executor, why they are waiting, and for how long.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newQueueLsCmd())
	return c
}

func newQueueLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List queued items (GET /queue/api/json)",
		Args:  cobra.NoArgs,
		Example: `  muxcat jenkins queue ls
  muxcat jenkins queue ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET",
				"/queue/api/json?tree=items[id,task[fullName,url],why,blocked,buildable,stuck,inQueueSince]", nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			var rows [][]any
			if m, ok := raw.(map[string]any); ok {
				if items, ok := m["items"].([]any); ok {
					for _, item := range items {
						q, ok := item.(map[string]any)
						if !ok {
							continue
						}
						job := ""
						if task, ok := q["task"].(map[string]any); ok {
							job = str(task["fullName"])
						}
						rows = append(rows, []any{
							q["id"], job, str(q["why"]),
							queueWaiting(q["inQueueSince"]), queueState(q),
						})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"id", "job", "why", "waiting", "state"},
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
}

// queueState derives the one-word state of a queue item; stuck implies
// blocked, so the most specific flag wins.
func queueState(q map[string]any) string {
	switch {
	case boolOf(q["stuck"]):
		return "stuck"
	case boolOf(q["blocked"]):
		return "blocked"
	case boolOf(q["buildable"]):
		return "buildable"
	}
	return "pending"
}

// queueWaiting renders inQueueSince (millisecond epoch) as a humanized age.
func queueWaiting(v any) string {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(int64(f))).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return fmt.Sprint(d)
}

// boolOf reads a decoded JSON boolean field.
func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}
