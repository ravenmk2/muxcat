package openobserve

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newQueryAroundCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "around <stream>",
		Short: "Show records around a given timestamp (GET /api/{org}/{stream}/_around)",
		Long: `Show the records around an anchor timestamp (GET
/api/{org}/{stream}/_around). The server fixes a ±15 minute window
around --key; --size is the total record budget, split evenly
between both sides. --key (required) accepts unix microseconds,
RFC3339, a negative duration like -1h, or now, and need not hit a
real record. --size must be >= 2 (size=1 would mean unlimited
server-side).`,
		Args: cli.ExactArgs(1, "<stream>", "stream"),
		Example: `  muxcat openobserve query around app_logs --key 2026-07-03T12:00:00Z
  muxcat openobserve query around app_logs --key 1719900000000000 --size 20
  muxcat openobserve query around app_logs --key -5m --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			keyStr := cli.FlagString(cmd, "key")
			if keyStr == "" {
				return output.NewError(output.CodeMissingArgument,
					"missing required flag --key (usage: "+cmd.CommandPath()+" <stream> --key <timestamp>)",
					"the anchor timestamp: unix microseconds, RFC3339, a negative duration, or now")
			}
			now := time.Now()
			key, err := parseTime(keyStr, now)
			if err != nil {
				return err
			}
			size, _ := cmd.Flags().GetInt("size")
			if size < 2 {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("invalid --size value: %d", size),
					"minimum is 2 (size=1 would mean unlimited server-side: the around API treats size/2=0 as no limit)")
			}
			streamType := cli.FlagString(cmd, "type")
			if !validStreamType(streamType) {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --type value: "+streamType, "valid values: logs|metrics|traces")
			}

			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			q := url.Values{
				"key":  {fmt.Sprintf("%d", key.UnixMicro())},
				"size": {fmt.Sprintf("%d", size)},
			}
			if streamType != "logs" {
				q.Set("type", streamType)
			}
			// _around lives at /api/{org}/{stream}/_around, not under /streams/.
			path := fmt.Sprintf("/api/%s/%s/_around?%s", url.PathEscape(conn.org()), url.PathEscape(args[0]), q.Encode())
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}

			var parsed struct {
				Hits []json.RawMessage `json:"hits"`
			}
			if err := json.Unmarshal(resp.body, &parsed); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			cols, rows := hitsToTable(parsed.Hits)
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: json.RawMessage(resp.body),
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("key", "", "anchor timestamp: unix microseconds, RFC3339, a negative duration, or now (required)")
	c.Flags().Int("size", 10, "total record budget around the key (minimum 2; maps to the API's size field)")
	c.Flags().String("type", "logs", "stream type: logs|metrics|traces")
	return c
}
