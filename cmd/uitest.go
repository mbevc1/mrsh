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

// savedRun is one saved host's outcome: Err is set when its secrets could
// not be resolved (it was never contacted); otherwise Result holds the job's.
type savedRun struct {
	Target runner.Target
	Result runner.Result
	Err    error
}

// savedRunner runs a job on saved hosts for the UI. Each call reloads the
// config (so defaults edited in the UI apply), accepts exact saved names
// only, and resolves secrets per host so one missing secret fails only that
// host.
type savedRunner struct {
	cmd  *cobra.Command
	opts *globalOptions
	mu   sync.Mutex // loadConfig writes the resolved defaults into opts
}

// run executes job with at least minParallel workers; perHost bounds one
// host's work beyond the dial timeout.
func (s *savedRunner) run(ctx context.Context, names []string, minParallel int, perHost time.Duration, job runner.ConnJob) ([]savedRun, error) {
	s.mu.Lock()
	lc, err := loadConfig(s.cmd, s.opts)
	conn, parallel, timeout := connOptions(s.opts), max(s.opts.parallel, minParallel), s.opts.timeout
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	targets, err := namedTargets(lc.cfg, names)
	if err != nil {
		return nil, err
	}

	runs := make([]savedRun, len(targets))
	var ready []runner.Target
	var readyIdx []int
	resolver := newResolver()
	for i := range targets {
		runs[i].Target = targets[i]
		if err := runner.Resolve(ctx, resolver, targets[i:i+1]); err != nil {
			runs[i].Err = err
			continue
		}
		ready = append(ready, targets[i])
		readyIdx = append(readyIdx, i)
	}

	rounds := (len(ready) + parallel - 1) / parallel
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(rounds, 1))*(time.Duration(timeout)*time.Second+perHost))
	defer cancel()
	for j, r := range runner.Execute(ctx, ready, parallel, runner.WithConn(conn, job)) {
		runs[readyIdx[j]].Result = r
	}
	return runs, nil
}

// failure is the (summary, detail) pair for a host that failed: its secret
// error, or its job error.
func (r savedRun) failure() (string, string, bool) {
	err := r.Err
	if err == nil {
		err = r.Result.Err
	}
	if err == nil {
		return "", "", false
	}
	return shortError(err), strings.ReplaceAll(err.Error(), "\n", " "), true
}

// hostTester backs the UI's Test button: connect to each named host and run
// the RouterOS version check. A host that answers but is not RouterOS still
// passes as "connected".
func hostTester(cmd *cobra.Command, opts *globalOptions) webui.Tester {
	s := &savedRunner{cmd: cmd, opts: opts}
	return func(ctx context.Context, names []string) ([]webui.TestResult, error) {
		runs, err := s.run(ctx, names, minTestParallel, 30*time.Second, versionProbe)
		if err != nil {
			return nil, err
		}
		results := make([]webui.TestResult, len(runs))
		for i, r := range runs {
			out := &results[i]
			out.Name, out.Host, out.DurationMS = r.Target.Name, r.Target.Host.Host, r.Result.Duration.Milliseconds()
			if summary, detail, failed := r.failure(); failed {
				out.Summary, out.Detail = summary, detail
				continue
			}
			out.OK, out.Detail = true, "connected (not RouterOS)"
			if v, ok := r.Result.Data.(mt.Version); ok {
				out.Detail, out.Pending = describeVersion(v), v.FirmwarePending()
			}
			out.Summary = out.Detail
		}
		return results, nil
	}
}

// hostUpgrader backs the UI's Upgrade tab with the `mt upgrade` step: each
// device is re-planned on the spot (the page's view is never trusted), then
// the step runs, or with Dry is only described. Upgrades use --parallel as
// the CLI does.
func hostUpgrader(cmd *cobra.Command, opts *globalOptions) *webui.Upgrader {
	s := &savedRunner{cmd: cmd, opts: opts}
	run := func(dry bool) func(context.Context, []string) ([]webui.UpgradeResult, error) {
		return func(ctx context.Context, names []string) ([]webui.UpgradeResult, error) {
			minParallel := 1
			if dry {
				minParallel = minTestParallel
			}
			runs, err := s.run(ctx, names, minParallel, updateWait+60*time.Second,
				func(ctx context.Context, conn *ssh.Conn, _ runner.Target, res *runner.Result) {
					plan, err := planUpgrade(ctx, conn)
					if err == nil && !dry {
						err = applyUpgrade(ctx, conn, plan)
					}
					if err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					res.Data = plan
				})
			if err != nil {
				return nil, err
			}
			results := make([]webui.UpgradeResult, len(runs))
			for i, r := range runs {
				out := &results[i]
				out.Name, out.Host, out.DurationMS = r.Target.Name, r.Target.Host.Host, r.Result.Duration.Milliseconds()
				if summary, detail, failed := r.failure(); failed {
					out.Summary, out.Detail = summary, detail
					continue
				}
				plan, _ := r.Result.Data.(upgradePlan)
				out.OK, out.Action = true, plan.Action
				out.Summary = strings.ReplaceAll(plan.describe(dry), " -> ", " → ")
				out.Detail = out.Summary
			}
			return results, nil
		}
	}
	return &webui.Upgrader{Dry: run(true), Apply: run(false)}
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
