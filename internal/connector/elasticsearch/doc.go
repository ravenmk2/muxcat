package elasticsearch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newDocCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "doc",
		Short: "Read Elasticsearch documents",
		Long:  `Read documents by id. doc get fetches one document's _source.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newDocGetCmd())
	return c
}

func newDocGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <index> <id>",
		Short: "Get a document by id",
		Args:  cli.ExactArgs(2, "<index> <id>", "index", "id"),
		Example: `  muxcat es doc get app-logs 42
  muxcat es doc get app-logs 42 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			index, id := args[0], args[1]
			name, _, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET",
				"/"+url.PathEscape(index)+"/_doc/"+url.PathEscape(id), nil)
			if err != nil {
				if resp != nil && resp.status == http.StatusNotFound {
					e := output.ToError(err)
					return output.NewError(e.Code, e.Message,
						"document not found: check the index and id")
				}
				return err
			}
			var doc struct {
				Index   string         `json:"_index"`
				ID      string         `json:"_id"`
				Version int64          `json:"_version"`
				Source  map[string]any `json:"_source"`
			}
			if err := json.Unmarshal(resp.body, &doc); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			var generic any
			_ = json.Unmarshal(resp.body, &generic)
			var sb strings.Builder
			fmt.Fprintf(&sb, "_index: %s\n_id: %s\n_version: %d\n", doc.Index, doc.ID, doc.Version)
			if raw, err := json.MarshalIndent(doc.Source, "", "  "); err == nil {
				sb.Write(raw)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    sb.String(),
				JSONData: generic,
			}, meta(name, start, false))
		},
	}
}
