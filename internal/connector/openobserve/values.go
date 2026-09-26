package openobserve

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newQueryValuesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "values <stream>",
		Short: "List field values of a stream (GET /api/{org}/{stream}/_values)",
		Long: `List the values fields take within a time window (GET
/api/{org}/{stream}/_values) — e.g. "which values does level
have". --fields is required and comma-separated; the time window
flags match query and default to the last hour. Text modes merge
the response into one field, value, count table (count is empty
with --no-count).`,
		Args: cli.ExactArgs(1, "<stream>", "stream"),
		Example: `  muxcat openobserve query values app_logs --fields level --last 24h
  muxcat openobserve query values app_logs --fields level,code --keyword err
  muxcat openobserve query values app_logs --fields level --no-count --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			fields, err := parseFields(cli.FlagString(cmd, "fields"))
			if err != nil {
				return err
			}
			size, _ := cmd.Flags().GetInt("size")
			from, _ := cmd.Flags().GetInt("from")
			if size < 0 || from < 0 {
				return output.NewError(output.CodeConfigInvalid,
					"--size and --from must be >= 0", "")
			}
			streamType := cli.FlagString(cmd, "type")
			if !validStreamType(streamType) {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --type value: "+streamType, "valid values: logs|metrics|traces")
			}
			now := time.Now()
			startT, endT, err := resolveTimeWindow(cmd, now)
			if err != nil {
				return err
			}

			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			q := url.Values{
				"fields":     {fields},
				"size":       {strconv.Itoa(size)},
				"from":       {strconv.Itoa(from)},
				"start_time": {strconv.FormatInt(startT.UnixMicro(), 10)},
				"end_time":   {strconv.FormatInt(endT.UnixMicro(), 10)},
				"no_count":   {strconv.FormatBool(cli.FlagBool(cmd, "no-count"))},
			}
			if keyword := cli.FlagString(cmd, "keyword"); keyword != "" {
				q.Set("keyword", keyword)
			}
			if streamType != "logs" {
				q.Set("type", streamType)
			}
			path := fmt.Sprintf("/api/%s/%s/_values?%s", url.PathEscape(conn.org()), url.PathEscape(args[0]), q.Encode())
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}

			var parsed struct {
				Hits []struct {
					Field  string `json:"field"`
					Values []struct {
						Key any    `json:"zo_sql_key"`
						Num *int64 `json:"zo_sql_num"`
					} `json:"values"`
				} `json:"hits"`
			}
			if err := json.Unmarshal(resp.body, &parsed); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			cols := []string{"field", "value", "count"}
			var rows [][]any
			for _, h := range parsed.Hits {
				for _, v := range h.Values {
					var count any
					if v.Num != nil {
						count = *v.Num
					}
					rows = append(rows, []any{h.Field, v.Key, count})
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: json.RawMessage(resp.body),
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("fields", "", "comma-separated field names, e.g. level,code (required)")
	c.Flags().Int("size", 10, "maximum number of values per field, ordered by occurrence (maps to the API's size field)")
	c.Flags().Int("from", 0, "offset of the value list (maps to the API's from field)")
	c.Flags().String("keyword", "", "filter values by keyword")
	c.Flags().Bool("no-count", false, "omit occurrence counts (maps to the API's no_count field)")
	c.Flags().String("type", "logs", "stream type: logs|metrics|traces")
	c.Flags().String("start-time", "", "values start time: unix microseconds, RFC3339, or a negative duration like -1h")
	c.Flags().String("end-time", "", "values end time: unix microseconds, RFC3339, a negative duration, or now")
	c.Flags().String("last", "", "relative time window shortcut: "+lastValues+" (mutually exclusive with --start-time/--end-time)")
	return c
}

// parseFields validates and normalizes the required comma-separated
// --fields value.
func parseFields(raw string) (string, error) {
	parts := strings.Split(raw, ",")
	fields := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			fields = append(fields, s)
		}
	}
	if len(fields) == 0 {
		return "", output.NewError(output.CodeMissingArgument,
			"missing required flag --fields", "example: --fields level,code")
	}
	return strings.Join(fields, ","), nil
}
