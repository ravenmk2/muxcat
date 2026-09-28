package rabbitmq

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newDefinitionsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "definitions",
		Short: "Export and import broker definitions",
		Long: `Export and import broker definitions (users, vhosts,
permissions, queues, exchanges, bindings, policies — the whole
topology in one JSON document). This is the tool for migrating a
topology between environments or seeding a fresh broker.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newDefinitionsExportCmd(), newDefinitionsImportCmd())
	return c
}

func newDefinitionsExportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "export",
		Short: "Export broker definitions (GET /api/definitions)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq definitions export --json
  muxcat rabbitmq definitions export --file topology.json
  muxcat rabbitmq definitions export --vhost / --file vhost.json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			path := "/api/definitions"
			if cmd.Flags().Changed("vhost") {
				path += "/" + esc(vhostFlag(cmd))
			}
			resp, err := cl.do(cmd.Context(), "GET", path, nil)
			if err != nil {
				return err
			}
			raw, err := decodeBody(resp.body)
			if err != nil {
				return err
			}
			if file := cli.FlagString(cmd, "file"); file != "" {
				if err := os.WriteFile(file, resp.body, 0o600); err != nil {
					return output.NewError(output.CodeGeneral,
						"cannot write "+file+": "+err.Error(), "")
				}
				return cli.RenderResult(cmd, &output.Result{
					Value:   map[string]any{"file": file, "bytes": len(resp.body)},
					Message: fmt.Sprintf("exported definitions to %s (%d bytes)", file, len(resp.body)),
				}, meta(name, start, false))
			}
			pretty, err := json.MarshalIndent(raw, "", "  ")
			if err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    string(pretty),
				JSONData: raw,
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "export only this vhost's definitions (default: the whole broker)")
	c.Flags().String("file", "", "write the definitions to a file (0600) instead of stdout")
	return c
}

func newDefinitionsImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import",
		Short: "Import broker definitions (POST /api/definitions)",
		Long: `Import broker definitions from a JSON document, previously
produced by definitions export. The document is read from --file, or
from stdin when --file is omitted (or "-"). Importing merges into the
existing topology; it does not delete objects absent from the file.`,
		Args: cobra.NoArgs,
		Example: `  muxcat rabbitmq definitions import --file topology.json
  muxcat rabbitmq definitions import --vhost staging --file vhost.json
  cat topology.json | muxcat rmq definitions import`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := requireWritable(conn, "definitions import"); err != nil {
				return err
			}
			body, err := definitionsImportBody(cmd)
			if err != nil {
				return err
			}
			if !json.Valid(body) {
				return output.NewError(output.CodeConfigInvalid,
					"definitions input is not valid JSON", "pass a document produced by definitions export")
			}
			path := "/api/definitions"
			if cmd.Flags().Changed("vhost") {
				path += "/" + esc(vhostFlag(cmd))
			}
			if _, err := cl.do(cmd.Context(), "POST", path, body); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:   map[string]any{"imported": true, "bytes": len(body)},
				Message: fmt.Sprintf("imported definitions (%d bytes)", len(body)),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("vhost", "", "import into this vhost only (default: the whole broker)")
	c.Flags().String("file", "", "read the definitions from a file (- or omitted reads stdin)")
	return c
}

// definitionsImportBody reads the import document from --file; "-" or a
// missing flag means stdin.
func definitionsImportBody(cmd *cobra.Command) ([]byte, error) {
	file := cli.FlagString(cmd, "file")
	if file == "" || file == "-" {
		if cli.RuntimeFrom(cmd.Context()).TTY {
			return nil, output.NewError(output.CodeMissingArgument,
				"definitions import reads the document from stdin, but stdin is a terminal",
				"pipe the document in, or pass --file <path>")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read definitions from stdin: "+err.Error(), "")
		}
		return raw, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, output.NewError(output.CodeGeneral,
			"cannot read definitions file "+file+": "+err.Error(), "")
	}
	return raw, nil
}
