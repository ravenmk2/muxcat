package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/connector"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/schema"
)

// newConfigValidateCmd validates config documents. With no arguments it
// validates all known files present in the config directory (config.json
// plus each registered connector's <name>.json); with arguments it
// validates the given files, matching schemas by file base name.
func newConfigValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [file...]",
		Short: "Validate config files against the embedded JSON Schemas",
		Example: `  muxcat config validate                 # all config files in the config dir
  muxcat config validate redis.json      # specific files, matched by base name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			type target struct{ label, path string }
			var targets []target
			if len(args) == 0 {
				dir, err := config.Dir()
				if err != nil {
					return err
				}
				names := append([]string{config.MainFile}, connectorNamesConfigFiles()...)
				for _, n := range names {
					p := filepath.Join(dir, n)
					if _, err := os.Stat(p); err == nil {
						targets = append(targets, target{n, p})
					}
				}
				if len(targets) == 0 {
					return RenderResult(cmd, &output.Result{
						Message: "no validatable files in the config directory (" + dir + ")",
					}, output.Meta{})
				}
			} else {
				for _, a := range args {
					targets = append(targets, target{filepath.Base(a), a})
				}
			}

			rows := make([][]any, 0, len(targets))
			failures := 0
			for _, t := range targets {
				data, err := os.ReadFile(t.path)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						rows = append(rows, []any{t.label, "not found", ""})
						continue
					}
					return output.NewError(output.CodeConfigInvalid,
						"failed to read "+t.path+": "+err.Error(), "")
				}
				if schema.SchemaName(t.label) == "" {
					rows = append(rows, []any{t.label, "skipped", "no matching schema"})
					continue
				}
				if err := schema.Validate(t.label, data); err != nil {
					failures++
					rows = append(rows, []any{t.label, "invalid", output.ToError(err).Message})
					continue
				}
				rows = append(rows, []any{t.label, "ok", ""})
			}
			rerr := RenderResult(cmd, &output.Result{
				Columns: []string{"file", "status", "error"},
				Rows:    rows,
			}, output.Meta{})
			if rerr != nil {
				return rerr
			}
			if failures > 0 {
				return output.NewError(output.CodeConfigInvalid,
					"some config files failed validation", "fix the reported files and retry")
			}
			return nil
		},
	}
}

// connectorNamesConfigFiles returns the config file name convention
// (<name>.json) of all registered connectors.
func connectorNamesConfigFiles() []string {
	names := connector.Names()
	files := make([]string, 0, len(names))
	for _, n := range names {
		files = append(files, n+".json")
	}
	return files
}
