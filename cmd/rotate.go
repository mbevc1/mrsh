package cmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/ssh"
)

// rotation changes one password, shared by every target, on RouterOS
// devices and in SSM. All devices end up on the new password, or all stay
// on the old one.
type rotation struct {
	label   string // "credential routers" or "host rb02"
	dest    string // SSM parameter for the new password
	current string // ARN being replaced in place, or "" when dest is new
	hosts   []string
	// setARN points the config at a new dest; only called when current is "".
	setARN func(d *config.Document, cfg *config.Config, arn string) error
}

// rotateFlagsHelp is the --rotate-pass part of help.
const rotateFlagsHelp = "\n\n--rotate-pass generates a new password, sets it on every RouterOS device that uses it\n" +
	"(each confirms a fresh login with it), and stores it in SSM: in place when the password\n" +
	"is already an SSM parameter, otherwise where --pass-ssm says. If any device fails, the\n" +
	"others are set back to the old password and SSM is left as it was."

// passwordAlphabet avoids quotes and characters RouterOS or shells treat specially.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789-_"

func generatePassword() (string, error) {
	b := make([]byte, 24)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b), nil
}

// checkRotateOnly refuses other changes alongside --rotate-pass.
func checkRotateOnly(cmd *cobra.Command) error {
	for _, n := range []string{"user", "user-env", "user-arn", "user-ssm", "identity-file", "host", "group", "port", "credential"} {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return fmt.Errorf("--rotate-pass changes only the password; run --%s separately", n)
		}
	}
	return nil
}

// passSSMFlag is --pass-ssm's value, or "" when it was not given.
func passSSMFlag(cmd *cobra.Command, f *secretFlags) string {
	if cmd.Flags().Changed("pass-ssm") {
		return f.passSSM
	}
	return ""
}

// rotationDest picks the parameter for the new password: passSSM (a name,
// ssmDefault, or "" when not given), else the SSM parameter the password
// already lives in.
func rotationDest(passSSM string, cfg *config.Config, owner, name, label, currentARN string) (dest, current string, err error) {
	cur, curOK := config.SSMParamName(currentARN)
	if passSSM != "" {
		dest = passSSM
		if dest == ssmDefault {
			dest = cfg.SSMName(owner, name, "pass")
		}
		if err := config.ValidSSMName(dest); err != nil {
			return "", "", fmt.Errorf("--pass-ssm: %w", err)
		}
		if curOK && cur == dest {
			return dest, currentARN, nil
		}
		return dest, "", nil
	}
	if curOK {
		return cur, currentARN, nil
	}
	return "", "", fmt.Errorf("%s's password is not in SSM; add --pass-ssm to store the new one there", label)
}

func credentialRotation(cfg *config.Config, name, passSSM string) (*rotation, error) {
	set, ok := cfg.Credentials[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrCredentialNotFound, name)
	}
	r := &rotation{label: "credential " + name}
	for _, h := range cfg.Hosts {
		if cfg.PassSource(h) == r.label {
			r.hosts = append(r.hosts, h.Name)
		}
	}
	if len(r.hosts) == 0 {
		return nil, fmt.Errorf("no host takes its password from credential %s", name)
	}
	var err error
	if r.dest, r.current, err = rotationDest(passSSM, cfg, config.OwnerCredential, name, r.label, set.PassARN); err != nil {
		return nil, err
	}
	r.setARN = func(d *config.Document, cfg *config.Config, arn string) error {
		set := cfg.Credentials[name]
		set.Pass, set.PassEnv, set.PassARN = "", "", arn
		return d.SetCredential(name, set)
	}
	return r, nil
}

func hostRotation(cfg *config.Config, name, passSSM string) (*rotation, error) {
	h, ok := findHost(cfg, name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
	}
	r := &rotation{label: "host " + name, hosts: []string{name}}
	if src := cfg.PassSource(h); src != r.label && passSSM == "" {
		if strings.HasPrefix(src, "credential ") {
			return nil, fmt.Errorf("host %s uses the password of %s: rotate that with credentials set --name %s --rotate-pass, or give %s its own with --pass-ssm",
				name, src, strings.TrimPrefix(src, "credential "), name)
		}
		return nil, fmt.Errorf("host %s has no password of its own: add --pass-ssm to store the new one there", name)
	}
	var err error
	if r.dest, r.current, err = rotationDest(passSSM, cfg, config.OwnerHost, name, r.label, h.PassARN); err != nil {
		return nil, err
	}
	r.setARN = func(d *config.Document, cfg *config.Config, arn string) error {
		h, _ := findHost(cfg, name)
		h.Pass, h.PassEnv, h.PassARN = "", "", arn
		return d.ReplaceHost(name, h)
	}
	return r, nil
}

// rotationEngine is what a rotation needs to run, from the CLI or the UI.
type rotationEngine struct {
	co        runner.ConnOptions
	parallel  int
	w         config.SecretWriter
	store     config.ConfigStore
	overwrite bool // a new dest may replace an existing parameter
	// progress, if set, hears each device's step ("changing the password");
	// done, if set, hears each device's result as soon as it is known.
	progress func(t runner.Target, step string)
	done     func(t runner.Target, res runner.Result)
}

// rotationOutcome is how a rotation ended. Results hold each device's final
// line; Stuck devices could not be set back and still have the password
// kept in the Stage parameter.
type rotationOutcome struct {
	Results   []runner.Result
	Committed bool
	Failed    int
	Stuck     []string
	Stage     string
	Notes     []string
}

// summary is the one-line ending for the CLI and the UI.
func (r *rotation) summary(o rotationOutcome) string {
	switch {
	case o.Committed:
		return fmt.Sprintf("rotated the password of %s on %d device(s); stored in SSM as %s", r.label, len(o.Results), r.dest)
	case len(o.Stuck) > 0:
		return fmt.Sprintf("rotation failed and %s still have the new password; it is kept in SSM as %s", strings.Join(o.Stuck, ", "), o.Stage)
	}
	return fmt.Sprintf("rotation failed on %d of %d device(s); every device is back on the old password and SSM is unchanged", o.Failed, len(o.Results))
}

// resolveRotationTargets loads the targets with their current passwords.
func resolveRotationTargets(ctx context.Context, cfg *config.Config, r *rotation) ([]runner.Target, error) {
	targets, err := namedTargets(cfg, r.hosts)
	if err != nil {
		return nil, err
	}
	if err := runner.Resolve(ctx, newResolver(), targets); err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t.Secrets.Pass == "" {
			return nil, fmt.Errorf("host %s has no current password to rotate from", t.Name)
		}
	}
	return targets, nil
}

// execute runs the rotation: write the new password to SSM first (a
// staging parameter when replacing in place, so it is never only in
// memory), change it on every device, then commit or roll back. An error
// means nothing on the devices changed, or (from commit) that every device
// changed but recording it did not finish; the message says what to do.
func (r *rotation) execute(ctx context.Context, e rotationEngine, targets []runner.Target) (rotationOutcome, error) {
	step := func(t runner.Target, s string) {
		if e.progress != nil {
			e.progress(t, s)
		}
	}
	newPass, err := generatePassword()
	if err != nil {
		return rotationOutcome{}, err
	}

	// Write ahead, so the new password exists somewhere before any device has it.
	stage := r.dest
	if r.current != "" {
		stage = r.dest + "-next"
	}
	overwrote := r.current == "" && e.overwrite
	stageARN, err := e.w.PutSecret(ctx, stage, newPass, r.current != "" || e.overwrite)
	if err != nil {
		return rotationOutcome{}, err
	}
	if r.current != "" && arnScope(stageARN) != arnScope(r.current) {
		_ = e.w.DeleteSecret(ctx, stageARN)
		return rotationOutcome{}, fmt.Errorf("%s is in another account or region than this AWS session (%s); nothing was changed", r.current, arnScope(stageARN))
	}

	job := runner.WithConn(e.co,
		func(ctx context.Context, conn *ssh.Conn, t runner.Target, res *runner.Result) {
			res.Data = false // the device's password changed
			fail := func(err error) { res.Err, res.ExitCode = err, -1 }
			step(t, "checking it is RouterOS")
			if err := requireRouterOS(ctx, conn); err != nil {
				fail(err)
				return
			}
			step(t, "changing the password")
			if err := setPassword(ctx, conn, t.Secrets.Pass, newPass); err != nil {
				fail(fmt.Errorf("set password: %w", err))
				return
			}
			res.Data = true
			step(t, "checking a fresh login")
			fresh := t
			fresh.Secrets.Pass = newPass
			c, err := runner.Connect(ctx, fresh, e.co)
			if err != nil {
				fail(fmt.Errorf("password set, but a fresh login with it failed: %w", err))
				return
			}
			_ = c.Close()
			res.Stdout = "password changed"
		})
	results := runner.Execute(ctx, targets, e.parallel, func(ctx context.Context, t runner.Target) runner.Result {
		res := job(ctx, t)
		if e.done != nil {
			e.done(t, res)
		}
		return res
	})

	o := rotationOutcome{Results: results}
	for _, res := range results {
		if res.Failed() {
			o.Failed++
		}
	}
	if o.Failed == 0 && ctx.Err() == nil {
		// Every device has the new password: recording it must finish.
		return r.commit(context.WithoutCancel(ctx), e, o, newPass, stageARN)
	}
	return r.rollback(e, targets, o, newPass, stageARN, overwrote, step), nil
}

// commit stores the new password for good once every device has it.
func (r *rotation) commit(ctx context.Context, e rotationEngine, o rotationOutcome, newPass, stageARN string) (rotationOutcome, error) {
	o.Committed = true
	if r.current != "" {
		if _, err := e.w.PutSecret(ctx, r.dest, newPass, true); err != nil {
			return o, fmt.Errorf("every device has the new password, but storing it in %s failed: %w; it is in %s, so copy it over", r.dest, err, r.dest+"-next")
		}
		if err := e.w.DeleteSecret(ctx, stageARN); err != nil {
			o.Notes = append(o.Notes, fmt.Sprintf("could not delete the staging parameter %s-next: %v", r.dest, err))
		}
		return o, nil
	}
	err := config.Mutate(ctx, e.store, func(d *config.Document, cfg *config.Config) error { return r.setARN(d, cfg, stageARN) })
	if err != nil {
		return o, fmt.Errorf("%w\nevery device and SSM (%s) have the new password, but the config was not changed; point %s at it with --pass-arn %s", err, r.dest, r.label, stageARN)
	}
	return o, nil
}

// errNotRolledBack marks a device left on the new password.
var errNotRolledBack = errors.New("could not set the old password back")

// rollback sets every changed device back to its old password. If that
// fails anywhere, the staging parameter, which holds the new password, is kept.
func (r *rotation) rollback(e rotationEngine, targets []runner.Target, o rotationOutcome, newPass, stageARN string, overwrote bool, step func(runner.Target, string)) rotationOutcome {
	ctx := context.Background() // finish undoing even after Ctrl-C or a closed page
	o.Stage, _ = config.SSMParamName(stageARN)
	for i, res := range o.Results {
		if changed, _ := res.Data.(bool); !changed {
			if !res.Failed() {
				o.Results[i].Stdout = "unchanged: rotation stopped"
			}
			continue
		}
		step(targets[i], "setting the old password back")
		fresh := targets[i]
		fresh.Secrets.Pass = newPass
		err := func() error {
			c, err := runner.Connect(ctx, fresh, e.co)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			return setPassword(ctx, c, newPass, targets[i].Secrets.Pass)
		}()
		if err != nil {
			o.Stuck = append(o.Stuck, targets[i].Name)
			o.Results[i].Err, o.Results[i].ExitCode = fmt.Errorf("%w: %w", errNotRolledBack, err), -1
			continue
		}
		if !res.Failed() {
			o.Results[i].Stdout = "rolled back: another device failed"
		}
	}
	switch {
	case len(o.Stuck) > 0: // keep the staging parameter: it holds their password
	case overwrote:
		o.Notes = append(o.Notes, fmt.Sprintf("%s now holds a password no device uses (it was replaced with --overwrite)", o.Stage))
	default:
		_ = e.w.DeleteSecret(ctx, stageARN) // nothing has the new password any more
	}
	return o
}

// rotate is the CLI front end: dry run, confirmation, then execute, with
// the per-device table and the outcome as the exit status.
func (r *rotation) rotate(cmd *cobra.Command, opts *globalOptions, f *secretFlags, lc *loadedConfig) error {
	ctx := cmd.Context()
	targets, err := resolveRotationTargets(ctx, lc.cfg, r)
	if err != nil {
		return err
	}
	runner.WarnIfInsecure(opts.hostKeyPolicy, opts.hostKeyPolicySet, len(targets))
	out := cmd.OutOrStdout()
	if opts.dryRun {
		_, _ = fmt.Fprintf(out, "dry-run: would rotate the password of %s on %d device(s) and store it in SSM as %s:\n", r.label, len(targets), r.dest)
		for _, t := range targets {
			_, _ = fmt.Fprintf(out, "  %s (%s)\n", t.Name, t.Host.Host)
		}
		return nil
	}
	if !opts.yes {
		if err := confirmTargets(cmd, "rotate the password of "+r.label+" on", targets); err != nil {
			return err
		}
	}
	o, err := r.execute(ctx, rotationEngine{
		co: connOptions(opts), parallel: opts.parallel, w: newSecretWriter(), store: lc.store, overwrite: f.overwrite,
	}, targets)
	if err != nil && o.Results == nil {
		return err
	}
	if werr := runner.Write(out, opts.output, o.Results); werr != nil {
		return werr
	}
	if err != nil {
		return err
	}
	for _, n := range o.Notes {
		_, _ = fmt.Fprintln(out, "note: "+n)
	}
	switch {
	case o.Committed:
		_, err := fmt.Fprintln(out, r.summary(o))
		return err
	case ctx.Err() != nil && len(o.Stuck) == 0:
		return errInterrupted
	}
	return &exitCodeError{code: exitHostFailed, msg: r.summary(o)}
}

// setPassword changes the login user's password. Unlike runChecked, its
// errors never quote the command, which holds both passwords.
func setPassword(ctx context.Context, conn *ssh.Conn, old, next string) error {
	stdout, stderr, code, err := conn.Run(ctx, mt.PasswordCmd(old, next), nil)
	if err == nil && code != 0 {
		err = fmt.Errorf("/password exited %d: %s", code, strings.TrimSpace(stderr+stdout))
	}
	if err == nil {
		err = mt.CheckOutput(stdout + stderr)
	}
	return err
}

// arnScope is the partition, region and account part of an ARN.
func arnScope(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return arn
	}
	return strings.Join(parts[1:5], ":")
}

// requireRouterOS refuses a device that is not RouterOS: only RouterOS
// passwords can be rotated.
func requireRouterOS(ctx context.Context, conn *ssh.Conn) error {
	rb, _, _, err := conn.Run(ctx, mt.RouterboardCmd, nil)
	if err != nil {
		return err
	}
	script, _, _, err := conn.Run(ctx, mt.VersionScript, nil)
	if err != nil {
		return err
	}
	if _, err := mt.ParseVersion(rb, script); err != nil {
		return errors.New("not RouterOS: only RouterOS passwords can be rotated")
	}
	return nil
}
