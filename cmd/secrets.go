package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mbevc1/mrsh/internal/config"
)

// newSecretWriter builds the SSM writer; tests may replace it.
var newSecretWriter = config.NewSecretWriter

// ssmDefault is the value --user-ssm/--pass-ssm take when given no name:
// the parameter's default name, e.g. /mrsh/creds/routers/pass.
const ssmDefault = "default"

// wantsSSM reports whether any SSM-storing flag was given.
func (f *secretFlags) wantsSSM(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("user-ssm") || cmd.Flags().Changed("pass-ssm")
}

// store writes the values for --user-ssm/--pass-ssm to SSM before the
// config changes (a config edit may retry; a store must not), and keeps the
// ARNs for apply. The user comes from -u or a prompt; the password only from
// a hidden prompt or stdin, never from a flag.
func (f *secretFlags) store(cmd *cobra.Command, opts *globalOptions, cfg *config.Config, owner, name string) error {
	fl := cmd.Flags()
	if !f.wantsSSM(cmd) {
		if fl.Changed("overwrite") {
			return errors.New("--overwrite only applies with --user-ssm or --pass-ssm")
		}
		return nil
	}
	label := owner + " " + name
	if owner == config.OwnerDefaults {
		label = "defaults"
	}
	w := newSecretWriter()
	for _, x := range []struct {
		flag, field string
		param, arn  *string
	}{{"user-ssm", "user", &f.userSSM, &f.userSSMARN}, {"pass-ssm", "pass", &f.passSSM, &f.passSSMARN}} {
		if !fl.Changed(x.flag) {
			continue
		}
		param := *x.param
		if param == ssmDefault {
			param = cfg.SSMName(owner, name, x.field)
		}
		if err := config.ValidSSMName(param); err != nil {
			return fmt.Errorf("--%s: %w", x.flag, err)
		}
		var value string
		var err error
		switch {
		case x.field == "user" && fl.Changed("user"):
			value = opts.user
		case x.field == "user":
			value, err = f.readLine(cmd, "User for "+label+": ")
		default:
			value, err = f.readPassword(cmd, "Password for "+label+": ")
		}
		if err != nil {
			return err
		}
		arn, err := w.PutSecret(cmd.Context(), param, value, f.overwrite)
		if err != nil {
			return err
		}
		*x.arn = arn
		f.stored = append(f.stored, fmt.Sprintf("--%s-arn %s", x.field, arn))
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stored the %s for %s in SSM as %s\n", x.field, label, param)
	}
	return nil
}

// storedNote turns a config error after a successful store into one that
// says where the secret went, so nothing is lost.
func (f *secretFlags) storedNote(err error) error {
	if err == nil || len(f.stored) == 0 {
		return err
	}
	return fmt.Errorf("%w\nthe secret is stored in SSM but the config was not changed; point the entry at it with %s",
		err, strings.Join(f.stored, " "))
}

// input is stdin wrapped once, so several prompts can share it.
func (f *secretFlags) input(cmd *cobra.Command) *bufio.Reader {
	if f.in == nil {
		f.in = bufio.NewReader(cmd.InOrStdin())
	}
	return f.in
}

func (f *secretFlags) readLine(cmd *cobra.Command, prompt string) (string, error) {
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	line, err := f.input(cmd).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if v := strings.TrimRight(line, "\r\n"); v != "" {
		return v, nil
	}
	return "", errors.New("no value given")
}

// readPassword reads a password without echo from a terminal, asking twice;
// otherwise it takes the first line of stdin (for scripts).
func (f *secretFlags) readPassword(cmd *cobra.Command, prompt string) (string, error) {
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		v, err := f.readLine(cmd, "")
		if err != nil {
			return "", errors.New("no password on stdin")
		}
		return v, nil
	}
	first, err := hiddenRead(cmd, file, prompt)
	if err != nil {
		return "", err
	}
	again, err := hiddenRead(cmd, file, "Repeat it: ")
	if err != nil {
		return "", err
	}
	switch {
	case first == "":
		return "", errors.New("empty password")
	case first != again:
		return "", errors.New("the passwords did not match")
	}
	return first, nil
}

// hiddenRead reads one line without echo. Ctrl-C restores the terminal and
// ends the command as interrupted.
func hiddenRead(cmd *cobra.Command, file *os.File, prompt string) (string, error) {
	fd := int(file.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		done <- result{b, err}
	}()
	select {
	case <-cmd.Context().Done():
		_ = term.Restore(fd, state)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		return "", errInterrupted
	case r := <-done:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		return string(r.b), r.err
	}
}

// deleteSecrets removes the planned parameters after the config change.
func deleteSecrets(cmd *cobra.Command, cfg *config.Config, del, kept []string) error {
	out := cmd.OutOrStdout()
	for _, k := range kept {
		_, _ = fmt.Fprintln(out, k)
	}
	if len(del) == 0 {
		return nil
	}
	w := newSecretWriter()
	var errs []error
	for _, arn := range del {
		name, _ := cfg.ManagedSSMName(arn)
		if err := w.DeleteSecret(cmd.Context(), arn); err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(out, "deleted SSM parameter %s\n", name)
	}
	return errors.Join(errs...)
}
