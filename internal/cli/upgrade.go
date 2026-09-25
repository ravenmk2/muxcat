package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/upgrade"
)

// upgradeTheme holds the lipgloss styles for the upgrade command's TTY
// output, reusing the help template's palette. With color off every style
// is the identity.
type upgradeTheme struct {
	header  lipgloss.Style // purple: command hints
	version lipgloss.Style // blue: version numbers
	path    lipgloss.Style // blue: asset names and file paths
	success lipgloss.Style // green: success lines
}

func newUpgradeTheme(color bool) upgradeTheme {
	r := lipgloss.DefaultRenderer()
	if color {
		// The color decision is already made via ResolveColor; force the
		// profile so lipgloss's own detection cannot veto it.
		r.SetColorProfile(termenv.TrueColor)
	}
	style := func(hex string) lipgloss.Style {
		s := r.NewStyle()
		if color {
			return s.Foreground(lipgloss.Color(hex))
		}
		return s
	}
	return upgradeTheme{
		header:  style("#7D56F4"),
		version: style("#2D9CDB"),
		path:    style("#2D9CDB"),
		success: style("#04B575"),
	}
}

// upgradeDefaultTimeout bounds the whole upgrade command (release lookup,
// downloads and retries). Much longer than the query-oriented global
// default of 30s, since release assets are tens of MB. An explicit
// --timeout flag always wins.
const upgradeDefaultTimeout = 5 * time.Minute

// upgradeTimeout resolves the command timeout: an explicit --timeout flag,
// otherwise upgradeDefaultTimeout (props.defaults.timeout is query-oriented
// and ignored here).
func upgradeTimeout(cmd *cobra.Command) time.Duration {
	if cmd.Flags().Changed("timeout") {
		return FlagTimeout(cmd)
	}
	return upgradeDefaultTimeout
}

// newUpgradeCmd implements `muxcat upgrade`: self-update from GitHub
// releases. Confirmation follows the global contract: a huh prompt on a
// TTY unless --yes, and a hard UNSUPPORTED_OPERATION error off-TTY without
// --yes (never waiting for input). TTY table output is rendered with
// colored highlights; plain/tsv/json keep the standard contract.
func newUpgradeCmd(version string) *cobra.Command {
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade muxcat to the latest or a specified release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			rt := RuntimeFrom(cmd.Context())
			_, mode, err := resolveRenderer(cmd)
			if err != nil {
				return err
			}
			attempts, _ := cmd.Flags().GetInt("attempts")
			if attempts < 1 {
				return output.NewError(output.CodeConfigInvalid,
					"invalid --attempts value: must be at least 1", "")
			}

			color := output.ResolveColor(loadDefaults().color, FlagBool(cmd, "no-color"), rt.TTY)
			fancy := rt.TTY && mode == output.ModeTable
			theme := newUpgradeTheme(color)
			out := cmd.OutOrStdout()

			ctx, cancel := context.WithTimeout(cmd.Context(), upgradeTimeout(cmd))
			defer cancel()
			client := upgrade.NewClient()
			opts := upgrade.Options{
				Current:  version,
				Version:  FlagString(cmd, "version"),
				Attempts: attempts,
			}
			plan, err := upgrade.Resolve(ctx, client, opts)
			if err != nil {
				return err
			}
			meta := output.Meta{ElapsedMS: time.Since(start).Milliseconds()}

			if plan.UpToDate {
				msg := fmt.Sprintf("muxcat is already up to date (%s)", plan.From)
				if plan.Newer {
					msg = fmt.Sprintf("current version (%s) is newer than the latest release (%s)", plan.From, plan.To)
				}
				if fancy {
					_, _ = fmt.Fprintf(out, "%s\n", theme.success.Render("✓ "+msg))
					return nil
				}
				return RenderResult(cmd, &output.Result{Message: msg}, meta)
			}
			if FlagBool(cmd, "check") {
				if fancy {
					_, _ = fmt.Fprintf(out, "muxcat %s is available (current: %s)\nrun %s to upgrade\n",
						theme.version.Render(plan.To), theme.version.Render(plan.From),
						theme.header.Render("muxcat upgrade"))
					return nil
				}
				return RenderResult(cmd, &output.Result{Value: map[string]any{
					"current":          plan.From,
					"latest":           plan.To,
					"update_available": true,
				}}, meta)
			}

			if !FlagBool(cmd, "yes") {
				if !rt.Interactive {
					return output.NewError(output.CodeUnsupportedOperation,
						"upgrade requires confirmation but stdin is not interactive",
						"re-run with --yes to confirm the upgrade")
				}
				confirm := true
				form := huh.NewForm(huh.NewGroup(
					huh.NewConfirm().
						Title(fmt.Sprintf("Upgrade muxcat from %s to %s?", plan.From, plan.To)).
						Value(&confirm),
				))
				if err := form.Run(); err != nil {
					return err
				}
				if !confirm {
					return RenderResult(cmd, &output.Result{Message: "upgrade canceled"}, meta)
				}
			}

			if fancy {
				target, err := upgrade.TargetPath()
				if err != nil {
					return err
				}
				opts.Target = target
				_, _ = fmt.Fprintf(out, "%-10s%s\n", "upgrade:", theme.version.Render(plan.From+" → "+plan.To))
				_, _ = fmt.Fprintf(out, "%-10s%s\n", "package:", theme.path.Render(plan.Asset))
				_, _ = fmt.Fprintf(out, "%-10s%s\n", "location:", theme.path.Render(target))
			}

			bar := upgrade.NewBar(os.Stderr, rt.TTY && mode != output.ModeJSON, color)
			res, err := upgrade.Apply(ctx, client, plan, opts, bar.Update)
			bar.Finish()
			if err != nil {
				return err
			}
			meta.ElapsedMS = time.Since(start).Milliseconds()
			if fancy {
				_, _ = fmt.Fprintf(out, "%s\n", theme.success.Render("✓ upgrade complete"))
				if res.Backup != "" {
					_, _ = fmt.Fprintf(out, "%-10s%s\n", "backup:", theme.path.Render(res.Backup))
				}
				return nil
			}
			return RenderResult(cmd, &output.Result{Value: map[string]any{
				"from":         res.From,
				"to":           res.To,
				"asset":        res.Asset,
				"installed_to": res.InstalledTo,
				"backup":       res.Backup,
			}}, meta)
		},
	}
	c.Flags().Bool("check", false, "check for updates without upgrading")
	c.Flags().String("version", "", "target version (default: latest release)")
	c.Flags().Int("attempts", 10, "download attempts before giving up")
	return c
}
