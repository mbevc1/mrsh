// Package runner is the shared core: resolve config, filter targets, execute, format.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/ssh"
)

// Target is one host to run on: an effective (defaults-merged) config entry
// or a literal address from --host, plus its resolved secrets.
type Target struct {
	config.Host
	Literal bool
	Secrets config.Secrets
}

// TargetOptions are the CLI inputs that select and complete targets.
type TargetOptions struct {
	Group        string
	Hosts        []string // --host values: config name/address, or literal
	User         string   // --user, literal hosts only
	IdentityFile string   // --identity-file, literal hosts only
}

// Targets returns (hosts in Group) ∪ (resolved --host values), deduplicated.
// Config entries come first in config order, then literals in argument order.
// No secrets are resolved here.
func Targets(cfg *config.Config, o TargetOptions) ([]Target, error) {
	selected, unmatched := cfg.Select(o.Group, o.Hosts)
	var targets []Target
	seen := map[string]bool{}
	for _, h := range selected {
		key := "config:" + h.Name
		if !seen[key] {
			seen[key] = true
			targets = append(targets, Target{Host: cfg.Effective(h)})
		}
	}
	for _, v := range unmatched {
		slog.Debug(fmt.Sprintf("--host %q not found in config; treating as literal address", v))
		h, err := literalHost(v, o)
		if err != nil {
			return nil, err
		}
		h = cfg.Effective(h)
		key := "literal:" + h.User + "@" + net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, Target{Host: h, Literal: true})
	}
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	slog.Debug("targets selected", "count", len(targets), "group", o.Group, "hosts", strings.Join(names, ","))
	return targets, nil
}

// literalHost parses [user@]host[:port], [v6]:port or a bare IPv6 address.
// --user beats a parsed user@, which beats the defaults' user group.
func literalHost(v string, o TargetOptions) (config.Host, error) {
	h := config.Host{Name: v}
	rest := v
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		h.User, rest = rest[:i], rest[i+1:]
	}
	portStr := ""
	switch {
	case strings.HasPrefix(rest, "["):
		end := strings.Index(rest, "]")
		if end < 0 {
			return config.Host{}, fmt.Errorf("--host %q: missing ']'", v)
		}
		h.Host, portStr = rest[1:end], rest[end+1:]
		if portStr != "" {
			if !strings.HasPrefix(portStr, ":") {
				return config.Host{}, fmt.Errorf("--host %q: expected ':port' after ']'", v)
			}
			portStr = portStr[1:]
		}
	case strings.Count(rest, ":") == 1:
		h.Host, portStr, _ = strings.Cut(rest, ":")
	default: // hostname, IPv4, or bare IPv6
		h.Host = rest
	}
	if h.Host == "" || strings.ContainsAny(h.Host, " \t/") {
		return config.Host{}, fmt.Errorf("--host %q: invalid address", v)
	}
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return config.Host{}, fmt.Errorf("--host %q: invalid port %q", v, portStr)
		}
		h.Port = p
	}
	if o.User != "" {
		h.User = o.User
	}
	h.IdentityFile = o.IdentityFile
	return h, nil
}

// SecretResolver resolves user/pass references; *config.Resolver implements it.
type SecretResolver interface {
	Resolve(ctx context.Context, hosts []config.Host) ([]config.Secrets, error)
}

// Resolve fills in the secrets of targets. Call it only on the selected
// targets, so unrelated hosts never need their secrets available.
func Resolve(ctx context.Context, r SecretResolver, targets []Target) error {
	hosts := make([]config.Host, len(targets))
	for i, t := range targets {
		hosts[i] = t.Host
	}
	secrets, err := r.Resolve(ctx, hosts)
	if err != nil {
		return err
	}
	for i := range targets {
		targets[i].Secrets = secrets[i]
	}
	return nil
}

// Result is the outcome on one host.
type Result struct {
	Name     string        `json:"name"`
	Host     string        `json:"host"`
	Group    string        `json:"group"`
	Port     int           `json:"port"`
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	Duration time.Duration `json:"-"`
	Err      error         `json:"-"`
}

// Failed reports whether the host errored or exited non-zero.
func (r Result) Failed() bool { return r.Err != nil || r.ExitCode != 0 }

// Job runs work on one target.
type Job func(ctx context.Context, t Target) Result

// Execute runs job on every target with at most parallel concurrent jobs.
// Results are index-aligned with targets. A job that panics becomes that
// host's error; other hosts are unaffected.
func Execute(ctx context.Context, targets []Target, parallel int, job Job) []Result {
	if parallel < 1 {
		parallel = 1
	}
	results := make([]Result, len(targets))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	blocked := 0
	start := time.Now()

	for i, t := range targets {
		select {
		case sem <- struct{}{}:
		default:
			blocked++
			sem <- struct{}{}
		}
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			defer func() { <-sem }()
			begin := time.Now()
			defer func() {
				if p := recover(); p != nil {
					results[i] = Result{Err: fmt.Errorf("internal error: %v", p), ExitCode: -1}
				}
				fillMeta(&results[i], t)
				if results[i].Duration == 0 {
					results[i].Duration = time.Since(begin)
				}
			}()
			results[i] = job(ctx, t)
		}(i, t)
	}
	wg.Wait()
	slog.Debug("execution finished", "hosts", len(targets), "parallel", parallel,
		"semaphore_blocked", blocked, "wall", time.Since(start))
	return results
}

func fillMeta(r *Result, t Target) {
	r.Name, r.Host, r.Group, r.Port = t.Name, t.Host.Host, t.Group, t.Port
}

// CommandOptions configure RunCommand.
type CommandOptions struct {
	Command  string
	Stdin    []byte // fed to each host's session when non-nil
	Timeout  time.Duration
	HostKeys *ssh.HostKeyChecker
	Policy   string
}

// RunCommand returns a Job that dials the target, runs the command and
// closes the connection.
func RunCommand(o CommandOptions) Job {
	return func(ctx context.Context, t Target) Result {
		start := time.Now()
		res := Result{ExitCode: -1}
		verifier, err := o.HostKeys.ForPolicy(o.Policy, t.KnownHosts)
		if err != nil {
			res.Err = err
			return res
		}
		conn, err := ssh.Dial(ctx, ssh.Config{
			Host:     t.Host.Host,
			Port:     t.Port,
			User:     loginUser(t.Secrets.User),
			Pass:     t.Secrets.Pass,
			KeyFile:  t.IdentityFile,
			Timeout:  o.Timeout,
			HostKeys: verifier,
		})
		if err != nil {
			res.Err, res.Duration = err, time.Since(start)
			return res
		}
		defer func() { _ = conn.Close() }()

		var stdin io.Reader // a nil *bytes.Reader would not be a nil io.Reader
		if o.Stdin != nil {
			stdin = bytes.NewReader(o.Stdin)
		}
		res.Stdout, res.Stderr, res.ExitCode, res.Err = conn.Run(ctx, o.Command, stdin)
		res.Duration = time.Since(start)
		return res
	}
}

// loginUser falls back to the local username, as OpenSSH does.
func loginUser(u string) string {
	if u != "" {
		return u
	}
	if cur, err := user.Current(); err == nil {
		return cur.Username
	}
	return ""
}

// WarnIfInsecure logs one warning per run when host keys go unchecked.
func WarnIfInsecure(policy string, targets int) {
	if targets > 0 && (policy == ssh.PolicyInsecure || policy == "") {
		slog.Warn("host key checking disabled (host_key_policy=insecure); use --host-key-policy strict or accept-new to verify hosts")
	}
}
