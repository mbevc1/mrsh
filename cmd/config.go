package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/mbevc1/mrsh/internal/config"
)

// loadedConfig is a validated config plus where it came from.
type loadedConfig struct {
	cfg     *config.Config
	store   config.ConfigStore
	version string
}

// loadConfig loads, parses and validates the --config source, then applies
// the precedence CLI flag > MRSH_DEBUG > hosts.yaml defaults > built-in
// default to the global options.
func loadConfig(cmd *cobra.Command, opts *globalOptions) (*loadedConfig, error) {
	store, err := config.NewStore(opts.config)
	if err != nil {
		return nil, err
	}
	raw, version, err := store.Load(cmd.Context())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config %s not found (create one with 'mrsh hosts init -f %s')", store.Location(), opts.config)
	}
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", store.Location(), err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", store.Location(), err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s:\n%w", store.Location(), err)
	}
	if err := applyDefaults(cmd, opts, raw); err != nil {
		return nil, err
	}
	slog.Debug("config loaded", "path", store.Location(), "source", sourceKind(opts.config),
		"hosts", len(cfg.Hosts), "parallel", opts.parallel, "timeout", opts.timeout, "output", opts.output)
	return &loadedConfig{cfg: cfg, store: store, version: version}, nil
}

// defaultBindings maps hosts.yaml defaults keys to global flag names.
var defaultBindings = map[string]string{
	"defaults.parallel": "parallel",
	"defaults.timeout":  "timeout",
	"defaults.output":   "output",
	"defaults.debug":    "debug",
}

// applyDefaults resolves the flag-or-config settings through viper. Viper
// never updates the bound flag variables, so the effective values are read
// back with viper.GetX and written into opts.
func applyDefaults(cmd *cobra.Command, opts *globalOptions, raw []byte) error {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("read config defaults: %w", err)
	}
	for key, flag := range defaultBindings {
		if err := v.BindPFlag(key, cmd.Flags().Lookup(flag)); err != nil {
			return err
		}
	}
	if err := v.BindEnv("defaults.debug", "MRSH_DEBUG"); err != nil {
		return err
	}

	opts.parallel = v.GetInt("defaults.parallel")
	opts.timeout = v.GetInt("defaults.timeout")
	opts.output = v.GetString("defaults.output")
	debug := v.GetBool("defaults.debug")
	if debug != opts.debug {
		opts.debug = debug
		setupLogging(cmd.ErrOrStderr(), debug)
	}
	if opts.parallel < 1 {
		return fmt.Errorf("parallel must be at least 1, got %d", opts.parallel)
	}
	if opts.timeout < 1 {
		return fmt.Errorf("timeout must be at least 1 second, got %d", opts.timeout)
	}
	return validateOutput(opts.output)
}

func sourceKind(uri string) string {
	if strings.HasPrefix(uri, "s3://") {
		return "s3"
	}
	return "local"
}
