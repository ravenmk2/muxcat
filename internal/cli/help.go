package cli

import (
	"os"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/output"
)

// helpTheme holds the lipgloss styles used by the help template.
// Colors are true-color values; lipgloss degrades them to the terminal's
// actual color profile.
type helpTheme struct {
	header lipgloss.Style
	name   lipgloss.Style
	flag   lipgloss.Style
}

func newHelpTheme(r *lipgloss.Renderer) helpTheme {
	return helpTheme{
		header: r.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")),
		name:   r.NewStyle().Foreground(lipgloss.Color("#04B575")),
		flag:   r.NewStyle().Foreground(lipgloss.Color("#2D9CDB")),
	}
}

func (t helpTheme) headerText(s string) string { return t.header.Render(s) }
func (t helpTheme) nameText(s string) string   { return t.name.Render(s) }

// flagTokenRe matches flag tokens (-c, --conn) in pflag FlagUsages output.
var flagTokenRe = regexp.MustCompile(`(^|\s)(--?[a-zA-Z][\w-]*)`)

// flags highlights the flag tokens in a FlagUsages block.
func (t helpTheme) flags(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range flagTokenRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:m[4]])
		b.WriteString(t.flag.Render(s[m[4]:m[5]]))
		last = m[5]
	}
	b.WriteString(s[last:])
	return b.String()
}

// helpColor decides color for help output at render time, reusing the
// ResolveColor decision chain (props.defaults.color + --no-color + NO_COLOR,
// non-TTY forces no color).
func helpColor(cmd *cobra.Command) bool {
	// Flags are parsed on the executed command; subcommands enumerated by
	// the template (Available Commands etc.) have no merged flag set, so
	// always read flags from the root command.
	root := cmd.Root()
	if root == nil {
		root = cmd
	}
	defColor := loadDefaults().color
	return output.ResolveColor(defColor, FlagBool(root, "no-color"), helpTTY())
}

// helpTTY reports whether the process runs on a TTY (stdin and stdout are
// both terminals). It deliberately checks the process stdio instead of
// cmd.OutOrStdout(): cobra's UsageString() renders into a temporary
// bytes.Buffer during help output, so the command's writer is not a
// reliable TTY signal.
func helpTTY() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// registerHelpTemplate installs the colored usage template on root
// (inherited by all subcommands; cobra's default help template renders the
// usage string through it). The color decision is made per template
// function call at render time, so flags like --no-color are honored even
// though they parse after init. With color off every style function is the
// identity, keeping the output byte-identical to cobra's default.
func registerHelpTemplate(root *cobra.Command) {
	cobra.AddTemplateFunc("styleHeader", func(cmd *cobra.Command, s string) string {
		if !helpColor(cmd) {
			return s
		}
		return newHelpTheme(lipgloss.DefaultRenderer()).headerText(s)
	})
	cobra.AddTemplateFunc("styleName", func(cmd *cobra.Command, s string) string {
		if !helpColor(cmd) {
			return s
		}
		return newHelpTheme(lipgloss.DefaultRenderer()).nameText(s)
	})
	cobra.AddTemplateFunc("styleFlags", func(cmd *cobra.Command, s string) string {
		if !helpColor(cmd) {
			return s
		}
		return newHelpTheme(lipgloss.DefaultRenderer()).flags(s)
	})
	root.SetUsageTemplate(usageTemplate)
}

// usageTemplate is cobra v1.10's defaultUsageTemplate with style hooks
// around section titles, command names, and flag tokens. Keep in sync when
// upgrading cobra.
const usageTemplate = `{{styleHeader . "Usage:"}}{{if .Runnable}}
  {{styleName . .UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{styleName . .CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

{{styleHeader . "Aliases:"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{styleHeader . "Examples:"}}
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

{{styleHeader . "Available Commands:"}}{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{styleName . (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{styleHeader $ $group.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{styleName . (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

{{styleHeader . "Additional Commands:"}}{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{styleName . (rpad .Name .NamePadding)}} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{styleHeader . "Flags:"}}
{{styleFlags . (.LocalFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasAvailableInheritedFlags}}

{{styleHeader . "Global Flags:"}}
{{styleFlags . (.InheritedFlags.FlagUsages | trimTrailingWhitespaces)}}{{end}}{{if .HasHelpSubCommands}}

{{styleHeader . "Additional help topics:"}}{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{styleName . (rpad .CommandPath .CommandPathPadding)}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{styleName . .CommandPath}} [command] --help" for more information about a command.{{end}}
`
