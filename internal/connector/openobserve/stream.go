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

// openForCmd resolves the connection for a data command and builds an
// authenticated client with the effective timeout.
func openForCmd(cmd *cobra.Command) (string, Connection, *client, time.Duration, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	name, conn, err := resolve(cfg, cli.FlagString(cmd, "conn"))
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	timeout, err := queryTimeout(conn, cli.FlagTimeout(cmd))
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	cl, err := newClient(cfg, conn, timeout)
	if err != nil {
		return "", Connection{}, nil, 0, err
	}
	return name, conn, cl, timeout, nil
}

// decodeBody parses a response body as JSON into a generic value.
func decodeBody(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, output.NewError(output.CodeQueryError,
			"response is not valid JSON: "+err.Error(), "")
	}
	return v, nil
}

// applyLimit truncates rows to the global --limit (0 disables truncation).
func applyLimit(rows [][]any, limit int) ([][]any, bool) {
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// validStreamType checks the --type enum of the streams APIs.
func validStreamType(t string) bool {
	switch t {
	case "logs", "metrics", "traces":
		return true
	}
	return false
}

func newStreamCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stream",
		Short: "Inspect OpenObserve streams",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newStreamLsCmd(), newStreamSchemaCmd())
	return c
}

func newStreamLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List streams (GET /api/{org}/streams)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			streamType := cli.FlagString(cmd, "type")
			if !validStreamType(streamType) {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --type value: "+streamType, "valid values: logs|metrics|traces")
			}
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/%s/streams?type=%s&fetchSchema=%t",
				url.PathEscape(conn.org()), streamType, cli.FlagBool(cmd, "fetch-schema"))
			ctx := cmd.Context()
			resp, err := cl.do(ctx, "GET", path, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}

			cols := []string{"name", "stream_type", "storage_type", "doc_num", "storage_size", "compressed_size"}
			var rows [][]any
			if m, ok := raw.(map[string]any); ok {
				if list, ok := m["list"].([]any); ok {
					for _, item := range list {
						s, ok := item.(map[string]any)
						if !ok {
							continue
						}
						stats, _ := s["stats"].(map[string]any)
						rows = append(rows, []any{
							s["name"], s["stream_type"], s["storage_type"],
							stats["doc_num"], stats["storage_size"], stats["compressed_size"],
						})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("type", "logs", "stream type: logs|metrics|traces")
	c.Flags().Bool("fetch-schema", false, "include each stream's schema in the response")
	return c
}

func newStreamSchemaCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "schema <name>",
		Short: "Show a stream's schema (GET /api/{org}/streams/{name}/schema)",
		Args:  cli.ExactArgs(1, "<name>", "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			streamType := cli.FlagString(cmd, "type")
			if !validStreamType(streamType) {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --type value: "+streamType, "valid values: logs|metrics|traces")
			}
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/%s/streams/%s/schema?type=%s",
				url.PathEscape(conn.org()), url.PathEscape(args[0]), streamType)
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}

			cols := []string{"name", "type"}
			var rows [][]any
			if m, ok := raw.(map[string]any); ok {
				if fields, ok := m["schema"].([]any); ok {
					for _, f := range fields {
						field, ok := f.(map[string]any)
						if !ok {
							continue
						}
						rows = append(rows, []any{field["name"], field["type"]})
					}
				}
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: raw,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().String("type", "logs", "stream type: logs|metrics|traces")
	return c
}
