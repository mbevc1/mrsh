// Package cmd wires the mrsh cobra commands to the internal packages.
package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// globalOptions holds the persistent flags shared by every subcommand.
type globalOptions struct {
	config       string
	group        string
	hosts        []string
	user         string
	identityFile string
	parallel     int
	timeout      int
	output       string
	dryRun       bool
	debug        bool
}

var validOutputs = []string{"text", "json", "csv"}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root, _ := newRootCmdWithOptions()
	return root
}

// newRootCmdWithOptions also returns the options the flags bind to, for tests.
func newRootCmdWithOptions() (*cobra.Command, *globalOptions) {
	opts := &globalOptions{}

	root := &cobra.Command{
		Use:           "mrsh",
		Short:         "Multi Remote SHell: run commands over SSH on many hosts in parallel",
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			setupLogging(cmd.ErrOrStderr(), opts.debug || envDebug())
			return validateOutput(opts.output)
		},
	}

	// Register global flags on the root only; re-registering them on a
	// subcommand silently breaks inheritance.
	pf := root.PersistentFlags()
	pf.StringVarP(&opts.config, "config", "f", "hosts.yaml", "config source: local path or s3://bucket/key")
	pf.StringVarP(&opts.group, "group", "g", "", "filter hosts by group")
	// -h is reserved for --help, so --host uses -H. --hosts is normalized to
	// --host (see flagAliases) rather than registered as a second flag: two
	// slice flags sharing one variable would each reset it on first use.
	pf.StringSliceVarP(&opts.hosts, "host", "H", nil, "target host(s): config name/address, or literal address if not in config (repeatable, comma-separated; alias --hosts)")
	pf.StringVarP(&opts.user, "user", "u", "", "SSH user for literal (non-config) hosts")
	pf.StringVarP(&opts.identityFile, "identity-file", "i", "", "SSH key for literal (non-config) hosts")
	pf.IntVarP(&opts.parallel, "parallel", "p", 1, "number of parallel SSH sessions")
	pf.IntVarP(&opts.timeout, "timeout", "t", 30, "SSH timeout in seconds")
	pf.StringVarP(&opts.output, "output", "o", "text", "output format: "+strings.Join(validOutputs, "|"))
	pf.BoolVar(&opts.dryRun, "dry-run", false, "print what would run without executing")
	pf.BoolVarP(&opts.debug, "debug", "d", false, "verbose logging to stderr")

	root.SetGlobalNormalizationFunc(normalizeFlagName)

	root.AddCommand(newVersionCmd(opts), newHostsCmd(opts))
	return root, opts
}

// flagAliases maps alternate flag spellings to their canonical name.
var flagAliases = map[string]string{"hosts": "host"}

func normalizeFlagName(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if canonical, ok := flagAliases[name]; ok {
		name = canonical
	}
	return pflag.NormalizedName(name)
}

// setupLogging installs a text slog handler on w: Debug when enabled, else Warn.
// Logs go to stderr so stdout stays clean for --output json|csv.
func setupLogging(w io.Writer, debug bool) {
	level := slog.LevelWarn
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
}

// envDebug parses MRSH_DEBUG the same way viper does (strconv.ParseBool).
func envDebug() bool {
	v, _ := strconv.ParseBool(os.Getenv("MRSH_DEBUG"))
	return v
}

func validateOutput(o string) error {
	for _, v := range validOutputs {
		if o == v {
			return nil
		}
	}
	return fmt.Errorf("invalid --output %q: must be one of %s", o, strings.Join(validOutputs, "|"))
}
