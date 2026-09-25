package openobserve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newIngestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest telemetry data into OpenObserve",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newIngestLogsCmd())
	return c
}

func newIngestLogsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "logs <stream>",
		Short: "Ingest log records (POST /api/{org}/{stream}/_json or _multi)",
		Args:  cli.ExactArgs(1, "<stream>", "stream"),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			format := cli.FlagString(cmd, "format")
			if format != "json" && format != "multi" {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --format value: "+format, "valid values: json|multi")
			}
			body, err := resolveIngestInput(cmd)
			if err != nil {
				return err
			}
			if err := validateIngestBody(format, body); err != nil {
				return err
			}

			name, conn, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if conn.Readonly {
				return output.NewError(output.CodeReadonlyViolation,
					"ingest is not allowed on a readonly connection",
					"use a writable connection (-c), or recreate the connection without --readonly")
			}
			path := fmt.Sprintf("/api/%s/%s/_%s", url.PathEscape(conn.org()), url.PathEscape(args[0]), format)
			resp, err := cl.do(cmd.Context(), "POST", path, body)
			if err != nil {
				return err
			}

			var parsed struct {
				Status []struct {
					Name       string `json:"name"`
					Successful int64  `json:"successful"`
					Failed     int64  `json:"failed"`
					Error      string `json:"error"`
				} `json:"status"`
			}
			if err := json.Unmarshal(resp.body, &parsed); err != nil {
				return output.NewError(output.CodeQueryError,
					"response is not valid JSON: "+err.Error(), "")
			}
			cols := []string{"stream", "successful", "failed", "error"}
			var rows [][]any
			var failedTotal int64
			for _, s := range parsed.Status {
				failedTotal += s.Failed
				rows = append(rows, []any{s.Name, s.Successful, s.Failed, s.Error})
			}
			// A completed exchange is reported (exit 0) even with partial
			// failures; surface them on stderr in text modes only (the
			// JSON envelope stays free of side notes).
			if failedTotal > 0 {
				mode, merr := output.ResolveMode(cli.FlagString(cmd, "output"),
					cli.FlagBool(cmd, "json"), "", cli.RuntimeFrom(cmd.Context()).TTY)
				if merr != nil || mode != output.ModeJSON {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %d record(s) failed to ingest\n", failedTotal)
				}
			}
			return cli.RenderResult(cmd, &output.Result{
				Columns:  cols,
				Rows:     rows,
				JSONData: json.RawMessage(resp.body),
			}, meta(name, start, false))
		},
	}
	c.Flags().String("file", "", "read the records from a file (- reads stdin; stdin is also the default when piped)")
	c.Flags().String("format", "json", "payload format: json (JSON array, _json endpoint) | multi (NDJSON lines, _multi endpoint)")
	return c
}

// resolveIngestInput reads the ingest payload: --file <path>, --file -
// (stdin), or stdin implicitly when it is piped. With no --file and a
// terminal stdin there is no data source.
func resolveIngestInput(cmd *cobra.Command) ([]byte, error) {
	file := cli.FlagString(cmd, "file")
	if file != "" && file != "-" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, output.NewError(output.CodeGeneral,
				"cannot read records file "+file+": "+err.Error(), "")
		}
		return raw, nil
	}
	// file == "-" or no --file: stdin is the source, but only when it is
	// not a terminal.
	if stdinIsCharDevice(cmd) {
		if file == "-" {
			return nil, output.NewError(output.CodeMissingArgument,
				"--file - reads the records from stdin, but stdin is a terminal",
				"pipe the records in, or pass a file path")
		}
		return nil, output.NewError(output.CodeMissingArgument,
			"no input (usage: "+cmd.CommandPath()+" <stream> --file <path>, or pipe records on stdin)", "")
	}
	raw, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return nil, output.NewError(output.CodeGeneral,
			"cannot read records from stdin: "+err.Error(), "")
	}
	return raw, nil
}

// stdinIsCharDevice reports whether the command's stdin is a terminal (or
// another character device), i.e. there is no piped input. Input injected
// via SetIn (tests, embedded use) never counts as a terminal.
func stdinIsCharDevice(cmd *cobra.Command) bool {
	in := cmd.InOrStdin()
	if in != os.Stdin {
		return false
	}
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// validateIngestBody pre-checks the payload client-side: json must be a
// JSON array; multi must be valid JSON on every non-empty line.
func validateIngestBody(format string, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return output.NewError(output.CodeConfigInvalid, "input is empty", "")
	}
	switch format {
	case "json":
		var records []json.RawMessage
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return output.NewError(output.CodeConfigInvalid,
				"input must be a JSON array of records: "+err.Error(),
				"the _json endpoint expects a JSON array; use --format multi for NDJSON lines")
		}
	case "multi":
		for i, line := range bytes.Split(trimmed, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if !json.Valid(line) {
				return output.NewError(output.CodeConfigInvalid,
					fmt.Sprintf("line %d is not valid JSON", i+1),
					"the _multi endpoint expects one JSON object per line")
			}
		}
	}
	return nil
}
