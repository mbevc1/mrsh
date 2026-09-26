// Package cmd wires the mrsh cobra commands to the internal packages.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mbevc1/mrsh/internal/config"
)

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

	hostKeyPolicy    string
	hostKeyPolicySet bool // chosen by flag or config, not the built-in default
	knownHosts       string

	sseMode string
	kmsKey  string
}

var validOutputs = []string{"text", "full", "json", "csv"}

// Exit codes: 0 all hosts ok, 1 any host failed, 2 usage or config error.
const (
	exitHostFailed  = 1
	exitUsage       = 2
	exitInterrupted = 130 // 128 + SIGINT, the shell convention
)

// exitCodeError carries a specific process exit code out of a command.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// Execute runs the root command and exits non-zero on error. SIGINT and
// SIGTERM cancel the context, which kills in-flight remote commands.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	os.Exit(exitCode(err))
}

func exitCode(err error) int {
	var ec *exitCodeError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ec):
		return ec.code
	}
	return exitUsage
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
	pf.StringVar(&opts.hostKeyPolicy, "host-key-policy", config.HostKeyInsecure, "host key checking: strict|accept-new|insecure")
	pf.StringVar(&opts.knownHosts, "known-hosts", "~/.ssh/known_hosts", "known_hosts file for strict/accept-new")
	pf.StringVar(&opts.sseMode, "sse", "", "S3 server-side encryption for writes (config saves, backups): AES256|aws:kms")
	pf.StringVar(&opts.kmsKey, "kms-key", "", "KMS key ARN for SSE-KMS (implies --sse aws:kms)")

	root.SetGlobalNormalizationFunc(normalizeFlagName)

	// The version text contains no "{{", so it works as a template verbatim.
	info := currentVersionInfo()
	root.Version = info.Version
	root.SetVersionTemplate(versionText(info))
	root.Flags().BoolP("version", "v", false, "print version and build info (same as 'mrsh version')")

	root.CompletionOptions.DisableDefaultCmd = true // replaced by newCompletionCmd
	root.AddCommand(newVersionCmd(opts), newHostsCmd(opts), newRunCmd(opts), newMtCmd(opts), newUICmd(opts), newCompletionCmd())
	for flag, fn := range map[string]cobra.CompletionFunc{
		"host":            completeHostNames(opts),
		"group":           completeGroups(opts),
		"output":          fixedValues(validOutputs...),
		"host-key-policy": fixedValues(config.HostKeyStrict, config.HostKeyAcceptNew, config.HostKeyInsecure),
		"sse":             fixedValues("AES256", "aws:kms"),
	} {
		_ = root.RegisterFlagCompletionFunc(flag, fn)
	}
	return root, opts
}

var flagAliases = map[string]string{"hosts": "host"}

func normalizeFlagName(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if canonical, ok := flagAliases[name]; ok {
		name = canonical
	}
	return pflag.NormalizedName(name)
}

// setupLogging logs to w (stderr) so stdout stays clean for --output json|csv.
func setupLogging(w io.Writer, debug bool) {
	if debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return
	}
	slog.SetDefault(slog.New(&plainHandler{w: w, mu: &sync.Mutex{}}))
}

// plainHandler prints warnings and errors as "mrsh: warning: msg k=v", a
// CLI-style line without the timestamp and level keys of debug logs.
type plainHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	attrs []slog.Attr
}

func (h *plainHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *plainHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString("mrsh: ")
	if r.Level >= slog.LevelError {
		b.WriteString("error: ")
	} else {
		b.WriteString("warning: ")
	}
	b.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *plainHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &plainHandler{w: h.w, mu: h.mu, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *plainHandler) WithGroup(string) slog.Handler { return h }

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
