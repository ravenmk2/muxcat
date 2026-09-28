package rabbitmq

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/cli"
	"github.com/ravenmk2/muxcat/internal/output"
)

func newFeatureFlagsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "featureflags",
		Short: "Inspect feature flags",
		Long: `Inspect feature flags: the capabilities of the broker and
whether they are enabled, disabled or unsupported. Feature flags are
enabled with rabbitmqctl on the server side; this group is read-only.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newFeatureFlagsLsCmd())
	return c
}

func newFeatureFlagsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List feature flags (GET /api/feature-flags)",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq featureflags ls
  muxcat rabbitmq featureflags ls --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/feature-flags", nil)
			if err != nil {
				return err
			}
			arr, err := decodeArray(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				f := obj(item)
				rows = append(rows, []any{
					str(f["name"]), str(f["state"]), str(f["stability"]),
					str(f["provided_by"]), str(f["desc"]),
				})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "state", "stability", "provided_by", "description"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

func newDeprecatedFeaturesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "deprecatedfeatures",
		Short: "Inspect deprecated features (requires RabbitMQ >= 3.13)",
		Long: `Inspect deprecated features: all features the server has
marked for removal, or only the ones actually in use. Requires
RabbitMQ >= 3.13; on older servers the endpoints answer 404, reported
as UNSUPPORTED_OPERATION.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newDeprecatedFeaturesLsCmd("ls", ""),
		newDeprecatedFeaturesLsCmd("used", "/used"),
	)
	return c
}

// newDeprecatedFeaturesLsCmd lists deprecated features, either all of them
// (suffix "") or only those in use (suffix "/used").
func newDeprecatedFeaturesLsCmd(what, suffix string) *cobra.Command {
	return &cobra.Command{
		Use:   what,
		Short: "List deprecated features (GET /api/deprecated-features" + suffix + ")",
		Args:  cobra.NoArgs,
		Example: `  muxcat rabbitmq deprecatedfeatures ` + what + `
  muxcat rabbitmq deprecatedfeatures ` + what + ` --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			name, _, cl, _, err := openForCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.do(cmd.Context(), "GET", "/api/deprecated-features"+suffix, nil)
			if err != nil {
				return err
			}
			arr, err := decodeDeprecatedFeatures(resp.body)
			if err != nil {
				return err
			}
			rows := make([][]any, 0, len(arr))
			for _, item := range arr {
				d := obj(item)
				state := str(d["state"])
				if state == "" {
					state = str(d["deprecation_phase"])
				}
				rows = append(rows, []any{str(d["name"]), state, str(d["desc"])})
			}
			rows, truncated := applyLimit(rows, cli.FlagLimit(cmd))
			return cli.RenderResult(cmd, &output.Result{
				Columns:  []string{"name", "state", "desc"},
				Rows:     rows,
				JSONData: arr,
			}, meta(name, start, truncated))
		},
	}
}

// decodeDeprecatedFeatures tolerates both response shapes: a bare array,
// or an object wrapping the list ("deprecated_features" / "used").
func decodeDeprecatedFeatures(raw []byte) ([]any, error) {
	v, err := decodeBody(raw)
	if err != nil {
		return nil, err
	}
	if arr, ok := v.([]any); ok {
		return arr, nil
	}
	if m, ok := v.(map[string]any); ok {
		for _, key := range []string{"deprecated_features", "used"} {
			if arr, ok := m[key].([]any); ok {
				return arr, nil
			}
		}
	}
	return nil, output.NewError(output.CodeQueryError,
		"response is not a JSON array", "the server answered with an unexpected shape")
}
