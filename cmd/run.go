package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/ssh"
)

// newResolver builds the secret resolver; tests replace it.
var newResolver = func() runner.SecretResolver { return config.NewResolver() }

func newRunCmd(opts *globalOptions) *cobra.Command {
	var command, script string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a command or script on hosts",
		Long: "Run a command (-c) or local script (--script, piped to 'sh -s') on the selected hosts.\n" +
			"With neither, the config's commands: list runs as one script.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lc, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}
			remote, stdin, err := commandToRun(command, script, lc.cfg.Commands)
			if err != nil {
				return err
			}
			targets, err := selectTargets(lc, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if opts.dryRun {
				return printDryRun(out, targets, remote, stdin)
			}

			ctx := cmd.Context()
			if err := prepareConnect(ctx, opts, targets); err != nil {
				return err
			}
			co := connOptions(opts)
			results := runner.Execute(ctx, targets, opts.parallel, runner.RunCommand(runner.CommandOptions{
				Command: remote, Stdin: stdin, Timeout: co.Timeout, HostKeys: co.HostKeys, Policy: co.Policy,
			}))
			if err := runner.Write(out, opts.output, results); err != nil {
				return err
			}
			return outcome(ctx, results)
		},
	}
	cmd.Flags().StringVarP(&command, "command", "c", "", "command to run")
	cmd.Flags().StringVar(&script, "script", "", "local script file to run remotely via 'sh -s'")
	cmd.MarkFlagsMutuallyExclusive("command", "script")
	return cmd
}

// selectTargets applies --group/--host/--user/--identity-file to the config.
func selectTargets(lc *loadedConfig, opts *globalOptions) ([]runner.Target, error) {
	targets, err := runner.Targets(lc.cfg, runner.TargetOptions{
		Group: opts.group, Hosts: opts.hosts, User: opts.user, IdentityFile: opts.identityFile,
	})
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("no target hosts (check --group/--host and the config)")
	}
	return targets, nil
}

// prepareConnect resolves secrets for the targets only, then warns once if
// host keys go unchecked.
func prepareConnect(ctx context.Context, opts *globalOptions, targets []runner.Target) error {
	if err := runner.Resolve(ctx, newResolver(), targets); err != nil {
		return err
	}
	runner.WarnIfInsecure(opts.hostKeyPolicy, len(targets))
	return nil
}

func connOptions(opts *globalOptions) runner.ConnOptions {
	return runner.ConnOptions{
		Timeout:  time.Duration(opts.timeout) * time.Second,
		HostKeys: &ssh.HostKeyChecker{},
		Policy:   opts.hostKeyPolicy,
	}
}

// outcome maps results to the process exit: interrupted, any host failed, or ok.
func outcome(ctx context.Context, results []runner.Result) error {
	if ctx.Err() != nil {
		return &exitCodeError{code: exitInterrupted, msg: "interrupted"}
	}
	failed := 0
	for _, r := range results {
		if r.Failed() {
			failed++
		}
	}
	if failed > 0 {
		return &exitCodeError{code: exitHostFailed, msg: fmt.Sprintf("%d of %d hosts failed", failed, len(results))}
	}
	return nil
}

// commandToRun picks the remote command and optional stdin: -c, then
// --script (streamed to "sh -s"), then the config's commands joined as a script.
func commandToRun(command, script string, configured []string) (string, []byte, error) {
	switch {
	case command != "":
		return command, nil, nil
	case script != "":
		body, err := os.ReadFile(script)
		if err != nil {
			return "", nil, fmt.Errorf("read script: %w", err)
		}
		return "sh -s", body, nil
	case len(configured) > 0:
		return strings.Join(configured, "\n"), nil, nil
	}
	return "", nil, errors.New("nothing to run: pass -c or --script, or set commands: in the config")
}

func printDryRun(w io.Writer, targets []runner.Target, remote string, stdin []byte) error {
	var b strings.Builder
	fmt.Fprintf(&b, "dry-run: would run on %d host(s):\n", len(targets))
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for _, t := range targets {
		kind := "config"
		if t.Literal {
			kind = "literal"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.Name, net.JoinHostPort(t.Host.Host, strconv.Itoa(t.Port)), kind)
	}
	_ = tw.Flush()
	b.WriteString("command:\n")
	if stdin != nil {
		fmt.Fprintf(&b, "  %s  (script, %d bytes on stdin)\n", remote, len(stdin))
	} else {
		for _, l := range strings.Split(remote, "\n") {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
