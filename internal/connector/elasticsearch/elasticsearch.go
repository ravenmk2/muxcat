// Package elasticsearch implements muxcat's Elasticsearch connector: a plain
// net/http client for the Elasticsearch REST API (basic auth or API key).
// Connection management, a connectivity probe (conn test), and raw request
// passthrough are covered; index/document sugar commands are out of scope
// for this iteration — use request to pass any endpoint through.
package elasticsearch

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
)

func init() {
	connector.Register("elasticsearch", New)
}

// New builds the elasticsearch connector's command tree.
func New() *cobra.Command {
	c := &cobra.Command{
		Use:     "elasticsearch",
		Aliases: []string{"es"},
		Short:   "Elasticsearch connector",
		Long: `Elasticsearch connector over the REST API, a plain net/http client
with no external driver. Supports Elasticsearch 7/8/9.

Quickstart:
  1. muxcat elasticsearch conn add local --url https://es.example.com:9200 --username elastic --password s3cret --set-default
  2. muxcat es conn test
  3. muxcat es request GET /_cat/indices
  4. muxcat es request POST /my-index/_search --file query.json

A connection carries credentials (basic auth or an API key, encrypted at
rest, never echoed) and policies: readonly allows GET/HEAD requests only.
The command name is elasticsearch (alias es). Index/document sugar
commands are out of scope — request passes any endpoint through.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConnCmd(),
		newRequestCmd(),
	)
	return c
}

// meta builds the envelope meta for elasticsearch commands.
func meta(connName string, start time.Time, truncated bool) output.Meta {
	return output.Meta{
		Connector:  "elasticsearch",
		Connection: connName,
		ElapsedMS:  time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}
}
