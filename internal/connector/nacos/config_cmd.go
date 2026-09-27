package nacos

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage Nacos configurations",
		Long: `Manage Nacos configurations. Configs are addressed by the
dataId/group/namespace triple: group defaults to DEFAULT_GROUP (-g
overrides), namespace to the connection's namespace (--namespace,
alias --ns, overrides). get prints the raw content; publish/delete are write
operations blocked on readonly connections; watch follows changes
(long-polling on 2.x, md5 polling on 3.x).

Quickstart:
  1. muxcat nacos config publish app.yaml --content "key: value"
  2. muxcat nacos config get app.yaml
  3. muxcat nacos config ls`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConfigGetCmd(),
		newConfigLsCmd(),
		newConfigPublishCmd(),
		newConfigDeleteCmd(),
		newConfigWatchCmd(),
	)
	return c
}

func newConfigGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <dataId>",
		Short: "Get a config's content",
		Long: `Get a config's content. Text output is the bare content,
syntax-highlighted on a TTY: the format reported by a 3.x server
wins, otherwise it is inferred from the dataId suffix (.yaml,
.json, ...), with a server-side lookup as the last resort (2.x).
--no-highlight disables the coloring; pipes are never colored.`,
		Args: cli.ExactArgs(1, "<dataId>", "dataId"),
		Example: `  muxcat nacos config get app.yaml
  muxcat nacos config get app.yaml -g BIZ_GROUP --namespace staging
  muxcat nacos config get app.yaml --no-highlight
  muxcat nacos config get app.yaml --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			dataID := args[0]
			group := groupOf(cmd)
			namespace := namespaceOf(cmd, conn)
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			content, format, err := cl.api().configGet(cmd.Context(), dataID, group, namespace)
			if err != nil {
				return err
			}
			if format == "" {
				format = inferFormat(dataID)
			}
			if format == "" {
				// Suffixless dataId on 2.x: ask the server (accurate
				// search); a failure just skips highlighting.
				format = cl.api().configType(cmd.Context(), dataID, group, namespace)
			}
			lexer := ""
			if !cli.FlagBool(cmd, "no-highlight") {
				lexer = highlightLexer(format)
			}
			return cli.RenderResult(cmd, &output.Result{
				Value:    map[string]any{"content": content},
				Bare:     true,
				Syntax:   lexer,
				JSONData: map[string]any{"dataId": dataID, "group": group, "namespace": namespace, "type": format, "content": content},
			}, meta(name, start, false))
		},
	}
	addAddressFlags(c)
	c.Flags().Bool("no-highlight", false, "disable syntax highlighting of the content (TTY only; pipes are never highlighted)")
	return c
}

// inferFormat guesses a config's format from the dataId suffix, matching
// the set of types Nacos infers on publish.
func inferFormat(dataID string) string {
	switch strings.ToLower(filepath.Ext(dataID)) {
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".properties", ".props":
		return "properties"
	case ".toml":
		return "toml"
	case ".txt", ".text", ".log":
		return "text"
	}
	return ""
}

// highlightLexer maps a Nacos config format to a chroma lexer name; ""
// means no highlighting.
func highlightLexer(format string) string {
	switch format {
	case "json", "yaml", "xml", "html", "toml":
		return format
	case "properties":
		return "ini"
	}
	return ""
}

func newConfigLsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ls",
		Short: "List configs (blur search) in the namespace",
		Args:  cobra.NoArgs,
		Example: `  muxcat nacos config ls
  muxcat nacos config ls --dataId app --limit 20
  muxcat nacos config ls -g BIZ_GROUP --namespace staging --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			group := cli.FlagString(cmd, "group")
			namespace := namespaceOf(cmd, conn)
			limit := cli.FlagLimit(cmd)
			pageSize := limit
			if pageSize <= 0 || pageSize > 500 {
				pageSize = 500
			}
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			page, err := cl.api().configList(cmd.Context(), cli.FlagString(cmd, "dataId"), group, namespace, 1, pageSize)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(page.Items))
			for _, it := range page.Items {
				rows = append(rows, []any{it.DataID, it.Group, it.Type})
			}
			rows, truncated := applyLimit(rows, limit)
			return cli.RenderResult(cmd, &output.Result{
				Columns:   []string{"dataId", "group", "type"},
				Rows:      rows,
				CellStyle: cellStyle,
			}, meta(name, start, truncated))
		},
	}
	c.Flags().StringP("group", "g", "", "filter by group (exact; empty matches all groups)")
	c.Flags().String("dataId", "", "filter by dataId (blur match; a bare word matches as a substring, * and ? wildcards supported)")
	addNamespaceFlag(c)
	return c
}

func newConfigPublishCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "publish <dataId>",
		Short: "Publish a config (creates or overwrites)",
		Long: `Publish a config (creates or overwrites). The content comes
from exactly one of --file (- reads stdin) or --content. --type
declares the config format (text, json, yaml, properties, ...);
the server infers it from the dataId suffix when omitted. Blocked
on readonly connections.`,
		Args: cli.ExactArgs(1, "<dataId>", "dataId"),
		Example: `  muxcat nacos config publish app.yaml --content "key: value" --type yaml
  muxcat nacos config publish app.yaml --file app.yaml
  cat app.json | muxcat nacos config publish app.json --file - -g BIZ_GROUP`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "config publish"); err != nil {
				return err
			}
			content, err := resolveContent(cmd)
			if err != nil {
				return err
			}
			dataID := args[0]
			group := groupOf(cmd)
			namespace := namespaceOf(cmd, conn)
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			if err := cl.api().configPublish(cmd.Context(), dataID, group, namespace, content, cli.FlagString(cmd, "type")); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("published config %s (%s, %s)", dataID, group, namespace),
				JSONData: map[string]any{"dataId": dataID, "group": group, "namespace": namespace, "published": true},
			}, meta(name, start, false))
		},
	}
	addAddressFlags(c)
	c.Flags().String("file", "", "read the config content from a file (- reads stdin)")
	c.Flags().String("content", "", "config content as a literal string")
	c.Flags().String("type", "", "config format: text|json|xml|yaml|html|properties (inferred from the dataId suffix when empty)")
	return c
}

func newConfigDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <dataId>",
		Short: "Delete a config",
		Args:  cli.ExactArgs(1, "<dataId>", "dataId"),
		Example: `  muxcat nacos config delete app.yaml
  muxcat nacos config delete app.yaml -g BIZ_GROUP --namespace staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			name, conn, cl, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			if err := guardWrite(conn, "config delete"); err != nil {
				return err
			}
			dataID := args[0]
			group := groupOf(cmd)
			namespace := namespaceOf(cmd, conn)
			if err := cl.ensureReady(cmd.Context()); err != nil {
				return err
			}
			if err := cl.api().configDelete(cmd.Context(), dataID, group, namespace); err != nil {
				return err
			}
			return cli.RenderResult(cmd, &output.Result{
				Message:  fmt.Sprintf("deleted config %s (%s, %s)", dataID, group, namespace),
				JSONData: map[string]any{"dataId": dataID, "group": group, "namespace": namespace, "deleted": true},
			}, meta(name, start, false))
		},
	}
	addAddressFlags(c)
	return c
}

// addAddressFlags adds the config addressing flags shared by the data
// commands.
func addAddressFlags(c *cobra.Command) {
	c.Flags().StringP("group", "g", "", "config group (default DEFAULT_GROUP)")
	addNamespaceFlag(c)
}

// resolveContent reads the publish content: exactly one of --file or
// --content is required; --file - reads stdin.
func resolveContent(cmd *cobra.Command) (string, error) {
	file := cli.FlagString(cmd, "file")
	content := cli.FlagString(cmd, "content")
	switch {
	case file != "" && content != "":
		return "", output.NewError(output.CodeMissingArgument,
			"--file and --content are mutually exclusive", "pass exactly one content source")
	case content != "":
		return content, nil
	case file == "-":
		if cli.RuntimeFrom(cmd.Context()).TTY {
			return "", output.NewError(output.CodeMissingArgument,
				"--file - reads the config content from stdin, but stdin is a terminal",
				"pipe the content in, or pass a file path")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read config content from stdin: "+err.Error(), "")
		}
		return string(raw), nil
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", output.NewError(output.CodeGeneral,
				"cannot read content file "+file+": "+err.Error(), "")
		}
		return string(raw), nil
	default:
		return "", output.NewError(output.CodeMissingArgument,
			"missing config content: pass --file <path> or --content <string>",
			"example: muxcat nacos config publish app.yaml --content \"key: value\"")
	}
}
