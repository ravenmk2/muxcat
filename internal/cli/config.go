package cli

import (
	"github.com/spf13/cobra"

	"github.com/ravenmk2/muxcat/internal/config"
	"github.com/ravenmk2/muxcat/internal/output"
	"github.com/ravenmk2/muxcat/internal/secret"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
		Long: `Manage muxcat's own configuration: global defaults in config.json
(get/set by dot path), the master key that encrypts stored credentials
(key), config file validation, and the config directory location
(path). Files live under MUXCAT_HOME (see: muxcat config path).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(
		newConfigPathCmd(),
		newConfigGetCmd(),
		newConfigSetCmd(),
		newConfigKeyCmd(),
		newConfigValidateCmd(),
	)
	return c
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "path",
		Short:   "Print the config directory path",
		Args:    cobra.NoArgs,
		Example: `  muxcat config path`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			return RenderResult(cmd, &output.Result{Message: dir}, output.Meta{})
		},
	}
}

func newConfigGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Read a config value by dot path (config.json paths are implicitly prefixed with props.)",
		Args:  ExactArgs(1, "<key>", "key"),
		Example: `  muxcat config get defaults.output
  muxcat config get defaults.timeout --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := config.Load(config.MainFile)
			if err != nil {
				return err
			}
			path := config.NormalizePath(config.MainFile, args[0])
			v, ok := config.Get(doc, path)
			if !ok {
				return output.NewError(output.CodeConfigInvalid,
					"config key not found: "+path, "set it with muxcat config set "+args[0]+" <value>")
			}
			return RenderResult(cmd, &output.Result{Value: v}, output.Meta{})
		},
	}
}

func newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write a config value by dot path (value is parsed as JSON, falling back to string)",
		Args:  ExactArgs(2, "<key> <value>", "key", "value"),
		Example: `  muxcat config set defaults.output json
  muxcat config set defaults.limit 500`,
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := config.Load(config.MainFile)
			if err != nil {
				return err
			}
			path := config.NormalizePath(config.MainFile, args[0])
			config.Set(doc, path, config.ParseValue(args[1]))
			if err := config.Save(config.MainFile, doc); err != nil {
				return err
			}
			return RenderResult(cmd, &output.Result{Message: "set " + path}, output.Meta{})
		},
	}
}

func newConfigKeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "key",
		Short: "Manage the master key",
		Long: `Manage the master key that encrypts all stored credentials (enc:v1:
blobs in connector configs). The key lives in the OS keychain, or in
the MUXCAT_KEY environment variable on keychain-less systems. The key
itself is never printed by any command.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newConfigKeyInitCmd(), newConfigKeyStatusCmd())
	return c
}

func newConfigKeyInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate a random 32-byte master key into the system keychain (existing key requires --yes to overwrite)",
		Args:  cobra.NoArgs,
		Example: `  muxcat config key init
  muxcat config key init --yes   # overwrite an existing key`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := secret.InitKey(FlagBool(cmd, "yes")); err != nil {
				return err
			}
			return RenderResult(cmd, &output.Result{Message: "master key generated and stored in the system keychain"}, output.Meta{})
		},
	}
}

func newConfigKeyStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report the master key's source and availability (never prints the key)",
		Args:  cobra.NoArgs,
		Example: `  muxcat config key status
  muxcat config key status --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := secret.Probe()
			if st.Source == secret.SourceNone {
				return output.NewError(output.CodeKeyUnavailable,
					"master key unavailable", "run muxcat config key init to generate one, or set the MUXCAT_KEY environment variable")
			}
			return RenderResult(cmd, &output.Result{Value: map[string]any{
				"source":             string(st.Source),
				"keychain_available": st.KeychainAvailable,
				"keychain_has_key":   st.KeychainHasKey,
				"env_set":            st.EnvSet,
				"env_valid":          st.EnvValid,
			}}, output.Meta{})
		},
	}
}
