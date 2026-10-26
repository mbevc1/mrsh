package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/ssh"
	"github.com/mbevc1/mrsh/internal/webui"
)

// minTestParallel keeps a "test all" in the UI quick even with --parallel 1.
const minTestParallel = 10

// hostTester backs the UI's Test button: it reloads the saved config (so
// defaults edited in the UI apply), then connects to each named host and
// runs the RouterOS version check. A host that answers but is not RouterOS
// still passes as "connected".
func hostTester(cmd *cobra.Command, opts *globalOptions) webui.Tester {
	var mu sync.Mutex // loadConfig writes the resolved defaults into opts
	return func(ctx context.Context, names []string) ([]webui.TestResult, error) {
		mu.Lock()
		lc, err := loadConfig(cmd, opts)
		conn, parallel, timeout := connOptions(opts), max(opts.parallel, minTestParallel), opts.timeout
		mu.Unlock()
		if err != nil {
			return nil, err
		}
		targets, err := namedTargets(lc.cfg, names)
		if err != nil {
			return nil, err
		}

		// Resolve per host, so one unset env var or missing secret fails
		// only that host's test.
		results := make([]webui.TestResult, len(targets))
		var ready []runner.Target
		var readyIdx []int
		resolver := newResolver()
		for i := range targets {
			results[i] = webui.TestResult{Name: targets[i].Name, Host: targets[i].Host.Host}
			if err := runner.Resolve(ctx, resolver, targets[i:i+1]); err != nil {
				results[i].Summary, results[i].Detail = shortError(err), err.Error()
				continue
			}
			ready = append(ready, targets[i])
			readyIdx = append(readyIdx, i)
		}

		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+30*time.Second)
		defer cancel()
		ran := runner.Execute(ctx, ready, parallel, runner.WithConn(conn, versionProbe))
		for j, r := range ran {
			out := &results[readyIdx[j]]
			out.DurationMS = r.Duration.Milliseconds()
			switch v, ok := r.Data.(mt.Version); {
			case r.Err != nil:
				out.Summary, out.Detail = shortError(r.Err), strings.ReplaceAll(r.Err.Error(), "\n", " ")
			case ok:
				out.OK, out.Detail = true, describeVersion(v)
			default:
				out.OK, out.Detail = true, "connected (not RouterOS)"
			}
			if out.OK {
				out.Summary = out.Detail
			}
		}
		return results, nil
	}
}

// shortError reduces a wrapped error to its cause for the UI badge, e.g.
// "dial 10.0.0.1:22: dial tcp 10.0.0.1:22: connect: no route to host"
// becomes "connect: no route to host". The full text stays in Detail.
func shortError(err error) string {
	var sys *os.SyscallError
	var dns *net.DNSError
	switch {
	case errors.As(err, &sys):
		return sys.Syscall + ": " + sys.Err.Error()
	case errors.As(err, &dns) && dns.IsNotFound:
		return "unknown host " + dns.Name
	case errors.As(err, &dns):
		return "lookup " + dns.Name + ": " + dns.Err
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timed out"
	case strings.Contains(err.Error(), "unable to authenticate"):
		return "authentication failed"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:] // the innermost wrapped message
	}
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i] // drop a trailing hint such as "(add it with ssh-keyscan, …)"
	}
	return s
}

// namedTargets returns the effective config entries for names, in config
// order (every host when names is empty). Names must match exactly, so the
// UI can never reach an address that is not a saved host.
func namedTargets(cfg *config.Config, names []string) ([]runner.Target, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var targets []runner.Target
	for _, h := range cfg.Hosts {
		if len(names) == 0 || want[h.Name] {
			targets = append(targets, runner.Target{Host: cfg.Effective(h)})
			delete(want, h.Name)
		}
	}
	for n := range want {
		return nil, fmt.Errorf("%w: %s", config.ErrHostNotFound, n)
	}
	return targets, nil
}

// versionProbe runs the read-only RouterOS version commands. Their output
// failing to parse is not an error: the host answered, it is just not
// RouterOS (on a Linux shell both commands simply fail).
func versionProbe(ctx context.Context, conn *ssh.Conn, _ runner.Target, res *runner.Result) {
	rb, _, _, err := conn.Run(ctx, mt.RouterboardCmd, nil)
	if err != nil {
		res.Err, res.ExitCode = err, -1
		return
	}
	script, _, _, err := conn.Run(ctx, mt.VersionScript, nil)
	if err != nil {
		res.Err, res.ExitCode = err, -1
		return
	}
	if v, err := mt.ParseVersion(rb, script); err == nil {
		res.Data = v
	}
}

func describeVersion(v mt.Version) string {
	parts := []string{"RouterOS " + v.ROS}
	if v.Board != "" {
		parts = append(parts, v.Board)
	}
	if v.FirmwarePending() {
		parts = append(parts, "firmware "+v.FirmwareRB+" → "+v.UpgradeFW+" pending")
	}
	return strings.Join(parts, " · ")
}
