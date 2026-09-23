package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
)

// Built-in defaults, used when neither the flag nor props.defaults
// specifies a value.
const (
	DefaultTimeout = 30 * time.Second
	DefaultLimit   = 1000
)

// FlagString reads a string flag, returning "" when undefined.
func FlagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// FlagBool reads a bool flag, returning false when undefined.
func FlagBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

// defaults holds the props.defaults values from config.json.
type defaults struct {
	output   string
	color    string
	timeout  string
	limit    float64
	hasLimit bool
}

// loadDefaults reads props.defaults from config.json. Missing or broken
// config yields zero values and never blocks the command.
func loadDefaults() defaults {
	doc, err := config.Load(config.MainFile)
	if err != nil {
		return defaults{}
	}
	d, _ := config.Get(doc, "props.defaults")
	m, _ := d.(map[string]any)
	var out defaults
	out.output, _ = m["output"].(string)
	out.color, _ = m["color"].(string)
	out.timeout, _ = m["timeout"].(string)
	out.limit, out.hasLimit = m["limit"].(float64)
	return out
}

// FlagTimeout resolves the command timeout: an explicit --timeout flag >
// props.defaults.timeout > DefaultTimeout.
func FlagTimeout(cmd *cobra.Command) time.Duration {
	if cmd.Flags().Changed("timeout") {
		if d, err := cmd.Flags().GetDuration("timeout"); err == nil {
			return d
		}
	}
	if s := loadDefaults().timeout; s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return DefaultTimeout
}

// FlagLimit resolves the query row limit: an explicit --limit flag >
// props.defaults.limit > DefaultLimit. 0 disables truncation.
func FlagLimit(cmd *cobra.Command) int {
	if cmd.Flags().Changed("limit") {
		if n, err := cmd.Flags().GetInt("limit"); err == nil {
			return n
		}
	}
	if d := loadDefaults(); d.hasLimit {
		return int(d.limit)
	}
	return DefaultLimit
}

// resolveRenderer resolves the output mode by flag > configured default >
// auto and constructs the Renderer.
func resolveRenderer(cmd *cobra.Command) (output.Renderer, output.Mode, error) {
	rt := RuntimeFrom(cmd.Context())
	defs := loadDefaults()
	mode, err := output.ResolveMode(FlagString(cmd, "output"), FlagBool(cmd, "json"), defs.output, rt.TTY)
	if err != nil {
		return nil, "", err
	}
	color := output.ResolveColor(defs.color, FlagBool(cmd, "no-color"), rt.TTY)
	return output.NewRenderer(mode, color), mode, nil
}

// RenderResult renders a command result uniformly: json mode wraps it in
// an envelope, other modes use the corresponding Renderer.
func RenderResult(cmd *cobra.Command, r *output.Result, meta output.Meta) error {
	renderer, mode, err := resolveRenderer(cmd)
	if err != nil {
		return err
	}
	if mode == output.ModeJSON {
		return output.WriteEnvelope(cmd.OutOrStdout(), output.Success(r.Payload(), meta))
	}
	return renderer.Render(cmd.OutOrStdout(), r)
}
