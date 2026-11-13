package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/webui"
)

// uiRotation is the rotation the page's Rotate button means: in place when
// the password is already in SSM, otherwise into the default parameter
// (so the config moves to SSM, as --pass-ssm would). A host must have its
// own password; one taken from a credential set is rotated with the set.
func uiRotation(cfg *config.Config, kind, name string) (*rotation, error) {
	inSSM := func(arn string) bool { _, ok := config.SSMParamName(arn); return ok }
	passSSM := ssmDefault
	switch kind {
	case "credential":
		if set, ok := cfg.Credentials[name]; ok && inSSM(set.PassARN) {
			passSSM = ""
		}
		return credentialRotation(cfg, name, passSSM)
	case "host":
		h, ok := findHost(cfg, name)
		if !ok {
			return nil, fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
		}
		switch src := cfg.PassSource(h); {
		case strings.HasPrefix(src, "credential "):
			return nil, fmt.Errorf("host %s uses the password of %s: rotate it there", name, src)
		case src != "host "+name:
			return nil, fmt.Errorf("host %s has no password of its own to rotate", name)
		case inSSM(h.PassARN):
			passSSM = ""
		}
		return hostRotation(cfg, name, passSSM)
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}

// hostRotator backs the page's Rotate buttons with the --rotate-pass engine.
// Each call reloads the config, so the page's view is never trusted.
// Rotations use --parallel as the CLI does.
func hostRotator(s *savedRunner) *webui.Rotator {
	load := func(kind, name string) (*rotation, *loadedConfig, error) {
		s.mu.Lock()
		lc, err := loadConfig(s.cmd, s.opts)
		s.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		r, err := uiRotation(lc.cfg, kind, name)
		return r, lc, err
	}
	return &webui.Rotator{
		Plan: func(_ context.Context, kind, name string) (webui.RotatePlan, error) {
			r, _, err := load(kind, name)
			if err != nil {
				return webui.RotatePlan{}, err
			}
			return webui.RotatePlan{Label: r.label, Hosts: r.hosts, Dest: r.dest, InPlace: r.current != ""}, nil
		},
		Apply: func(ctx context.Context, kind, name string, progress func(webui.RotateResult)) (webui.RotateOutcome, error) {
			if progress == nil {
				progress = func(webui.RotateResult) {}
			}
			r, lc, err := load(kind, name)
			if err != nil {
				return webui.RotateOutcome{}, err
			}
			targets, err := resolveRotationTargets(ctx, lc.cfg, r)
			if err != nil {
				return webui.RotateOutcome{}, err
			}
			s.mu.Lock()
			co, parallel, timeout := connOptions(s.opts), s.opts.parallel, s.opts.timeout
			s.mu.Unlock()
			// The page cannot interrupt a rotation, so a device that hangs
			// must not hold it forever; on timeout it rolls back.
			rounds := (len(targets) + max(parallel, 1) - 1) / max(parallel, 1)
			ctx, cancel := context.WithTimeout(ctx, time.Duration(rounds)*(3*time.Duration(timeout)*time.Second+time.Minute))
			defer cancel()

			o, err := r.execute(ctx, rotationEngine{
				co: co, parallel: parallel, w: newSecretWriter(), store: lc.store,
				progress: func(t runner.Target, step string) {
					progress(webui.RotateResult{Name: t.Name, Host: t.Host.Host, OK: true, Running: true, Summary: step})
				},
				done: func(t runner.Target, res runner.Result) {
					out := rotateResult(t, res)
					if out.OK { // final only once every device is done
						out.Running, out.Summary = true, "password changed; waiting for the other devices"
					}
					progress(out)
				},
			}, targets)
			if err != nil && o.Results == nil {
				return webui.RotateOutcome{}, err
			}
			out := webui.RotateOutcome{OK: o.Committed && err == nil, Message: r.summary(o), Notes: o.Notes}
			if err != nil {
				out.Message = err.Error()
			}
			for i, res := range o.Results {
				out.Results = append(out.Results, rotateResult(targets[i], res))
			}
			return out, nil
		},
	}
}

// rotateResult is one device's rotation line for the page.
func rotateResult(t runner.Target, res runner.Result) webui.RotateResult {
	out := webui.RotateResult{Name: t.Name, Host: t.Host.Host}
	if res.Err != nil {
		out.Summary, out.Detail = shortError(res.Err), strings.ReplaceAll(res.Err.Error(), "\n", " ")
		if errors.Is(res.Err, errNotRolledBack) {
			out.Summary = errNotRolledBack.Error() + ": " + out.Summary
		}
		return out
	}
	out.OK, out.Summary, out.Detail = true, res.Stdout, res.Stdout
	return out
}
