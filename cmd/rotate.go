package cmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/ssh"
)

// Where a rotated password lives. A rotation keeps it there: an SSM
// parameter is updated in place, a literal is replaced in the config, and
// an environment variable is for the user to set from the shown password.
const (
	storeSSM     = "ssm"
	storeLiteral = "literal"
	storeEnv     = "env"
)

// rotation changes one password, shared by every target, on RouterOS
// devices and where it is stored. All devices end up on the new password,
// or all stay on the old one.
type rotation struct {
	label   string // "credential routers" or "host rb02"
	hosts   []string
	store   string // storeSSM, storeLiteral or storeEnv
	dest    string // the SSM parameter name, or the environment variable
	current string // the SSM parameter's ARN
	// setLiteral writes the new literal into the config (storeLiteral).
	setLiteral func(d *config.Document, cfg *config.Config, pass string) error
}

// rotateFlagsHelp is the --rotate-pass part of help.
const rotateFlagsHelp = "\n\n--rotate-pass generates a new password, sets it on every RouterOS device that uses it\n" +
	"(each confirms a fresh login with it), and keeps it where the old one was: an SSM parameter\n" +
	"is updated in place, a literal is replaced in the config, and for an environment variable\n" +
	"the new password is printed for you to set. If any device fails, the others are set back to\n" +
	"the old password and nothing is stored. The new password is printed once when stdout is a\n" +
	"terminal (always for an environment variable)."

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
	fl := cmd.Flags()
	if fl.Changed("pass-ssm") || fl.Changed("overwrite") {
		return errors.New("--rotate-pass keeps the password where it is; to move it into SSM, run --pass-ssm on its own first")
	}
	for _, n := range []string{"user", "user-env", "user-arn", "user-ssm", "identity-file", "host", "group", "port", "credential"} {
		if f := fl.Lookup(n); f != nil && f.Changed {
			return fmt.Errorf("--rotate-pass changes only the password; run --%s separately", n)
		}
	}
	return nil
}

// newRotation reads where the password c holds lives. Secrets Manager is
// refused: mrsh does not write there.
func newRotation(label string, hosts []string, c config.Credentials) (*rotation, error) {
	r := &rotation{label: label, hosts: hosts}
	switch {
	case c.PassARN != "":
		name, ok := config.SSMParamName(c.PassARN)
		if !ok {
			return nil, fmt.Errorf("%s's password is not an SSM parameter (%s); mrsh rotates passwords in SSM, the config or an environment variable only", label, c.PassARN)
		}
		r.store, r.dest, r.current = storeSSM, name, c.PassARN
	case c.Pass != "":
		r.store = storeLiteral
	case c.PassEnv != "":
		r.store, r.dest = storeEnv, c.PassEnv
	default:
		return nil, fmt.Errorf("%s has no password of its own to rotate", label)
	}
	return r, nil
}

func credentialRotation(cfg *config.Config, name string) (*rotation, error) {
	set, ok := cfg.Credentials[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrCredentialNotFound, name)
	}
	label := "credential " + name
	var hosts []string
	for _, h := range cfg.Hosts {
		if cfg.PassSource(h) == label {
			hosts = append(hosts, h.Name)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no host takes its password from credential %s", name)
	}
	r, err := newRotation(label, hosts, set.Credentials)
	if err != nil {
		return nil, err
	}
	r.setLiteral = func(d *config.Document, cfg *config.Config, pass string) error {
		set := cfg.Credentials[name]
		set.Pass = pass
		return d.SetCredential(name, set)
	}
	return r, nil
}

func hostRotation(cfg *config.Config, name string) (*rotation, error) {
	h, ok := findHost(cfg, name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
	}
	label := "host " + name
	if src := cfg.PassSource(h); src != label {
		if set, ok := strings.CutPrefix(src, "credential "); ok {
			return nil, fmt.Errorf("host %s uses the password of %s: rotate it with credentials set --name %s --rotate-pass (or the Rotate button on the set)", name, src, set)
		}
		return nil, fmt.Errorf("host %s has no password of its own to rotate", name)
	}
	r, err := newRotation(label, []string{name}, h.Credentials)
	if err != nil {
		return nil, err
	}
	r.setLiteral = func(d *config.Document, cfg *config.Config, pass string) error {
		h, _ := findHost(cfg, name)
		h.Pass = pass
		return d.ReplaceHost(name, h)
	}
	return r, nil
}

// where says where the password is kept, for plans and summaries.
func (r *rotation) where() string {
	switch r.store {
	case storeSSM:
		return "SSM parameter " + r.dest
	case storeEnv:
		return "environment variable " + r.dest
	}
	return "the config"
}

// rotationEngine is what a rotation needs to run, from the CLI or the UI.
type rotationEngine struct {
	co       runner.ConnOptions
	parallel int
	w        config.SecretWriter
	store    config.ConfigStore
	// progress, if set, hears each device's step ("changing the password");
	// done, if set, hears each device's result as soon as it is known.
	progress func(t runner.Target, step string)
	done     func(t runner.Target, res runner.Result)
}

// rotationOutcome is how a rotation ended. Results hold each device's final
// line; Stuck devices could not be set back. NewPass is set when any device
// keeps the new password; MustShow means the output is its only copy.
type rotationOutcome struct {
	Results   []runner.Result
	Committed bool
	Failed    int
	Stuck     []string
	Stage     string // the SSM staging parameter kept for Stuck devices
	Notes     []string
	NewPass   string
	MustShow  bool
}

// summary is the one-line ending for the CLI and the UI.
func (r *rotation) summary(o rotationOutcome) string {
	switch {
	case o.Committed && r.store == storeEnv:
		return fmt.Sprintf("rotated the password of %s on %d device(s); set %s to the new password", r.label, len(o.Results), r.dest)
	case o.Committed:
		return fmt.Sprintf("rotated the password of %s on %d device(s); stored in %s", r.label, len(o.Results), r.where())
	case len(o.Stuck) > 0 && o.Stage != "":
		return fmt.Sprintf("rotation failed and %s the new password; it is kept in SSM as %s", stillHave(o.Stuck), o.Stage)
	case len(o.Stuck) > 0:
		return fmt.Sprintf("rotation failed and %s the new password, shown below; %s still holds the old one", stillHave(o.Stuck), r.where())
	}
	return fmt.Sprintf("rotation failed on %d of %d device(s); every device is back on the old password and %s is unchanged", o.Failed, len(o.Results), r.where())
}

// stillHave is "rt1 still has" or "rt1, rt2 still have".
func stillHave(names []string) string {
	if len(names) == 1 {
		return names[0] + " still has"
	}
	return strings.Join(names, ", ") + " still have"
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

// execute runs the rotation: for SSM, write the new password to a staging
// parameter first, so it is never only in memory; change it on every
// device; then commit or roll back. An error means nothing on the devices
// changed, or (from commit) that every device changed but recording it did
// not finish; the message says what to do, and the outcome has NewPass.
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

	var stageARN string
	if r.store == storeSSM {
		if stageARN, err = e.w.PutSecret(ctx, r.dest+"-next", newPass, true); err != nil {
			return rotationOutcome{}, err
		}
		if arnScope(stageARN) != arnScope(r.current) {
			_ = e.w.DeleteSecret(ctx, stageARN)
			return rotationOutcome{}, fmt.Errorf("%s is in another account or region than this AWS session (%s); nothing was changed", r.current, arnScope(stageARN))
		}
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
	return r.rollback(e, targets, o, newPass, stageARN, step), nil
}

// commit stores the new password where the old one was, once every device
// has it.
func (r *rotation) commit(ctx context.Context, e rotationEngine, o rotationOutcome, newPass, stageARN string) (rotationOutcome, error) {
	o.Committed, o.NewPass = true, newPass
	switch r.store {
	case storeSSM:
		if _, err := e.w.PutSecret(ctx, r.dest, newPass, true); err != nil {
			return o, fmt.Errorf("every device has the new password, but storing it in %s failed: %w; it is in %s-next, so copy it over", r.dest, err, r.dest)
		}
		if err := e.w.DeleteSecret(ctx, stageARN); err != nil {
			o.Notes = append(o.Notes, fmt.Sprintf("could not delete the staging parameter %s-next: %v", r.dest, err))
		}
	case storeLiteral:
		err := config.Mutate(ctx, e.store, func(d *config.Document, cfg *config.Config) error { return r.setLiteral(d, cfg, newPass) })
		if err != nil {
			o.MustShow = true
			return o, fmt.Errorf("%w\nevery device has the new password, but the config was not changed; set it on %s with --pass (it is shown below)", err, r.label)
		}
	case storeEnv:
		o.MustShow = true
		// This process (such as mrsh ui) keeps working with it; the shell's
		// copy is the user's to change.
		if err := os.Setenv(r.dest, newPass); err != nil {
			o.Notes = append(o.Notes, fmt.Sprintf("could not update %s for this process: %v", r.dest, err))
		}
	}
	return o, nil
}

// errNotRolledBack marks a device left on the new password.
var errNotRolledBack = errors.New("could not set the old password back")

// rollback sets every changed device back to its old password. If that
// fails anywhere, the new password is kept: in the SSM staging parameter,
// or for a literal or environment variable, in the outcome to be shown.
func (r *rotation) rollback(e rotationEngine, targets []runner.Target, o rotationOutcome, newPass, stageARN string, step func(runner.Target, string)) rotationOutcome {
	ctx := context.Background() // finish undoing even after Ctrl-C or a closed page
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
	case len(o.Stuck) > 0 && stageARN != "": // keep the staging parameter: it holds their password
		o.Stage, _ = config.SSMParamName(stageARN)
		o.NewPass = newPass
	case len(o.Stuck) > 0:
		o.NewPass, o.MustShow = newPass, true
	case stageARN != "":
		_ = e.w.DeleteSecret(ctx, stageARN) // nothing has the new password any more
	}
	return o
}

// rotate is the CLI front end: dry run, confirmation, then execute, with
// the per-device table and the outcome as the exit status.
func (r *rotation) rotate(cmd *cobra.Command, opts *globalOptions, lc *loadedConfig) error {
	ctx := cmd.Context()
	targets, err := resolveRotationTargets(ctx, lc.cfg, r)
	if err != nil {
		return err
	}
	runner.WarnIfInsecure(opts.hostKeyPolicy, opts.hostKeyPolicySet, len(targets))
	out := cmd.OutOrStdout()
	if opts.dryRun {
		_, _ = fmt.Fprintf(out, "dry-run: would rotate the password of %s on %d device(s), kept in %s:\n", r.label, len(targets), r.where())
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
	var w config.SecretWriter
	if r.store == storeSSM {
		w = newSecretWriter()
	}
	o, err := r.execute(ctx, rotationEngine{co: connOptions(opts), parallel: opts.parallel, w: w, store: lc.store}, targets)
	if err != nil && o.Results == nil {
		return err
	}
	if werr := runner.Write(out, opts.output, o.Results); werr != nil {
		return werr
	}
	for _, n := range o.Notes {
		_, _ = fmt.Fprintln(out, "note: "+n)
	}
	// Shown once: on a terminal, or always when nothing else keeps it.
	if o.NewPass != "" && (o.MustShow || isTerminal(out)) {
		_, _ = fmt.Fprintf(out, "new password for %s (shown once): %s\n", r.label, o.NewPass)
	}
	switch {
	case err != nil:
		return err
	case o.Committed:
		_, err := fmt.Fprintln(out, r.summary(o))
		return err
	case ctx.Err() != nil && len(o.Stuck) == 0:
		return errInterrupted
	}
	return &exitCodeError{code: exitHostFailed, msg: r.summary(o)}
}

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
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
