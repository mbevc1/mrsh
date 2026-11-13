package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

const sharedFlagsHelp = "The address, group, user and identity file come from the global flags:\n" +
	"  -H/--host ADDRESS, -g/--group GROUP, -u/--user USER, -i/--identity-file PATH.\n" +
	"Set at most one of --user-env/--user-arn/--user-ssm (or -u) and of --pass/--pass-env/--pass-arn/--pass-ssm.\n" +
	"--credential NAME uses a credential set (see mrsh credentials); fields set on the host win.\n\n" +
	ssmFlagsHelp

const ssmFlagsHelp = "--pass-ssm stores a password in AWS SSM Parameter Store (SecureString, AWS-managed key)\n" +
	"and references it by ARN. The password is read from a hidden prompt, or from stdin when\n" +
	"piped, never from a flag. --user-ssm does the same for the user (its value from -u or a\n" +
	"prompt). Without a name they use <ssm_prefix>/<creds|hosts>/<name>/<user|pass>, with\n" +
	"ssm_prefix from defaults (default /mrsh); --pass-ssm=/other/name picks another. An existing\n" +
	"parameter is only replaced with --overwrite."

// secretFlags are the user/pass variant flags shared by hosts add/update
// and credentials set. The literal user comes from the global -u.
type secretFlags struct {
	userEnv   string
	userARN   string
	userSSM   string
	pass      string
	passEnv   string
	passARN   string
	passSSM   string
	overwrite bool

	userSSMARN, passSSMARN string        // set by store
	stored                 []string      // "--pass-arn ARN" for each stored secret
	in                     *bufio.Reader // stdin, shared by prompts
}

func (f *secretFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.userEnv, "user-env", "", "read the user from this environment variable")
	fl.StringVar(&f.userARN, "user-arn", "", "read the user from this SSM or Secrets Manager ARN")
	fl.StringVar(&f.pass, "pass", "", "literal password (visible in shell history; prefer --pass-env or --pass-arn)")
	fl.StringVar(&f.passEnv, "pass-env", "", "read the password from this environment variable")
	fl.StringVar(&f.passARN, "pass-arn", "", "read the password from this SSM or Secrets Manager ARN")
	fl.StringVar(&f.userSSM, "user-ssm", "", "store the user in SSM (default name, or =/NAME) and reference it")
	fl.StringVar(&f.passSSM, "pass-ssm", "", "store a password, read from a hidden prompt or stdin, in SSM (default name, or =/NAME) and reference it")
	fl.Lookup("user-ssm").NoOptDefVal = ssmDefault
	fl.Lookup("pass-ssm").NoOptDefVal = ssmDefault
	fl.BoolVar(&f.overwrite, "overwrite", false, "replace an existing SSM parameter (with --user-ssm/--pass-ssm)")
	cmd.MarkFlagsMutuallyExclusive("pass", "pass-env", "pass-arn", "pass-ssm")
	cmd.MarkFlagsMutuallyExclusive("user-env", "user-arn", "user-ssm")
}

// apply copies supplied user/pass flags onto c; a supplied variant replaces
// that whole field-group. It reports whether anything changed.
func (f *secretFlags) apply(cmd *cobra.Command, opts *globalOptions, c *config.Credentials) (bool, error) {
	changed := false
	userFlags := map[string]string{"user": opts.user, "user-env": f.userEnv, "user-arn": f.userARN, "user-ssm": f.userSSMARN}
	if cmd.Flags().Changed("user-ssm") {
		delete(userFlags, "user") // -u was the value stored in SSM
	}
	if name, v, ok, err := oneOf(cmd, userFlags); err != nil {
		return false, err
	} else if ok {
		c.User, c.UserEnv, c.UserARN = "", "", ""
		switch name {
		case "user":
			c.User = v
		case "user-env":
			c.UserEnv = v
		case "user-arn", "user-ssm":
			c.UserARN = v
		}
		changed = true
	}
	passFlags := map[string]string{"pass": f.pass, "pass-env": f.passEnv, "pass-arn": f.passARN, "pass-ssm": f.passSSMARN}
	if name, v, ok, err := oneOf(cmd, passFlags); err != nil {
		return false, err
	} else if ok {
		if name == "pass" {
			slog.Warn("--pass stores the password in the config in the clear; prefer --pass-env or --pass-arn")
		}
		c.Pass, c.PassEnv, c.PassARN = "", "", ""
		switch name {
		case "pass":
			c.Pass = v
		case "pass-env":
			c.PassEnv = v
		case "pass-arn", "pass-ssm":
			c.PassARN = v
		}
		changed = true
	}
	return changed, nil
}

// hostFlags are the local flags of hosts add/update. The address, group,
// user and identity file reuse the global -H, -g, -u and -i flags, which
// have no other meaning for these commands.
type hostFlags struct {
	secretFlags
	name       string
	port       int
	credential string
}

func (f *hostFlags) register(cmd *cobra.Command, opts *globalOptions) {
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "host name (unique key)")
	fl.IntVar(&f.port, "port", 0, "SSH port (0 = use defaults)")
	fl.StringVar(&f.credential, "credential", "", "use this credential set; its user, pass and key apply where the host sets none (\"\" to clear)")
	f.secretFlags.register(cmd)
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.RegisterFlagCompletionFunc("credential", completeCredentials(opts))
}

// apply copies every supplied flag onto h and reports whether anything
// changed. A supplied user or pass variant replaces that whole field-group.
func (f *hostFlags) apply(cmd *cobra.Command, opts *globalOptions, h *config.Host) (bool, error) {
	fl := cmd.Flags()
	changed := false
	if fl.Changed("host") {
		if len(opts.hosts) != 1 {
			return false, fmt.Errorf("--host takes exactly one address here, got %d", len(opts.hosts))
		}
		h.Host, changed = opts.hosts[0], true
	}
	if fl.Changed("group") {
		h.Group, changed = opts.group, true
	}
	if fl.Changed("port") {
		h.Port, changed = f.port, true
	}
	if fl.Changed("credential") {
		h.Credential, changed = f.credential, true
	}
	if fl.Changed("identity-file") {
		h.IdentityFile, changed = opts.identityFile, true
	}
	secrets, err := f.secretFlags.apply(cmd, opts, &h.Credentials)
	return changed || secrets, err
}

// oneOf returns the single supplied flag among flags, erroring on several.
func oneOf(cmd *cobra.Command, flags map[string]string) (name, value string, ok bool, err error) {
	var set []string
	for n := range flags {
		if cmd.Flags().Changed(n) {
			set = append(set, "--"+n)
			name, value = n, flags[n]
		}
	}
	if len(set) > 1 {
		return "", "", false, fmt.Errorf("set only one of %s", strings.Join(set, ", "))
	}
	return name, value, len(set) == 1, nil
}

func newHostsAddCmd(opts *globalOptions) *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:     "add --name NAME -H ADDRESS [flags]",
		Aliases: []string{"a"},
		Short:   "Add a host entry",
		Long:    "Add a host entry.\n\n" + sharedFlagsHelp,
		Example: "  mrsh hosts add --name web03 -H 10.0.0.3 -g web -u deploy --pass-env WEB03_PASS",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("host") {
				return errors.New("hosts add needs the address: -H/--host ADDRESS")
			}
			if f.wantsSSM(cmd) {
				lc, err := loadConfig(cmd, opts)
				if err != nil {
					return err
				}
				if _, exists := findHost(lc.cfg, f.name); exists {
					return fmt.Errorf("host %s already exists (use hosts update)", f.name)
				}
				if err := f.store(cmd, opts, lc.cfg, config.OwnerHost, f.name); err != nil {
					return err
				}
			} else if err := f.store(cmd, opts, nil, "", ""); err != nil {
				return err
			}
			h := config.Host{Name: f.name}
			if _, err := f.apply(cmd, opts, &h); err != nil {
				return err
			}
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, _ *config.Config) error {
				return d.AddHost(h)
			})
			if err != nil {
				return f.storedNote(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "added host %s to %s\n", h.Name, store.Location())
			return err
		},
	}
	f.register(cmd, opts)
	return cmd
}

func newHostsUpdateCmd(opts *globalOptions) *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:     "update --name NAME [flags]",
		Aliases: []string{"u", "up"},
		Short:   "Update fields of a host entry (only the flags given)",
		Long: "Update fields of the host named by --name; only the flags given change.\n" +
			"A new user or pass variant replaces the old one.\n\n" + sharedFlagsHelp,
		Example: "  mrsh hosts update --name web03 -g db --pass-arn arn:aws:ssm:eu-west-1:123456789012:parameter/web03",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.wantsSSM(cmd) {
				lc, err := loadConfig(cmd, opts)
				if err != nil {
					return err
				}
				if _, ok := findHost(lc.cfg, f.name); !ok {
					return fmt.Errorf("%w: %s", config.ErrHostNotFound, f.name)
				}
				if err := f.store(cmd, opts, lc.cfg, config.OwnerHost, f.name); err != nil {
					return err
				}
			} else if err := f.store(cmd, opts, nil, "", ""); err != nil {
				return err
			}
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				h, ok := findHost(cfg, f.name)
				if !ok {
					return fmt.Errorf("%w: %s", config.ErrHostNotFound, f.name)
				}
				changed, err := f.apply(cmd, opts, &h)
				if err != nil {
					return err
				}
				if !changed {
					return errors.New("nothing to update: pass at least one field flag")
				}
				return d.ReplaceHost(f.name, h)
			})
			if err != nil {
				return f.storedNote(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated host %s in %s\n", f.name, store.Location())
			return err
		},
	}
	f.register(cmd, opts)
	_ = cmd.RegisterFlagCompletionFunc("name", completeNames(opts))
	return cmd
}

func newHostsRemoveCmd(opts *globalOptions) *cobra.Command {
	var name string
	var deleteSecret bool
	cmd := &cobra.Command{
		Use:     "remove --name NAME",
		Aliases: []string{"r", "rm"},
		Short:   "Remove a host entry (asks for confirmation unless -y/--assume-yes)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			asked := false
			var cfgAfter *config.Config
			var del, kept []string
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				h, ok := findHost(cfg, name)
				if !ok {
					return fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
				}
				del, kept, cfgAfter = nil, nil, cfg
				also := ""
				if deleteSecret {
					del, kept = plannedDeletes(cfg, h.Credentials, "host "+h.Name)
					if len(del) > 0 {
						also = fmt.Sprintf(" and delete %d SSM parameter(s)", len(del))
					}
				}
				if !opts.yes && !asked {
					asked = true
					if err := confirm(cmd, fmt.Sprintf("Remove host %s (%s) from %s%s? [y/N]: ", h.Name, h.Host, store.Location(), also)); err != nil {
						return err
					}
				}
				return d.RemoveHost(name)
			})
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "removed host %s from %s\n", name, store.Location()); err != nil {
				return err
			}
			return deleteSecrets(cmd, cfgAfter, del, kept)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "host name to remove")
	cmd.Flags().BoolVar(&deleteSecret, "delete-secret", false, "also delete the host's own SSM parameters under ssm_prefix that nothing else uses")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.RegisterFlagCompletionFunc("name", completeNames(opts))
	return cmd
}

func findHost(cfg *config.Config, name string) (config.Host, bool) {
	for _, h := range cfg.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return config.Host{}, false
}

// errAborted is a "no" at a confirmation prompt.
var errAborted = errors.New("aborted")

// confirm prompts on stderr and reads one line from stdin: y/yes returns
// nil, anything else (including EOF from a non-interactive stdin) returns
// errAborted. Ctrl-C while it waits ends the command as interrupted; the
// read itself cannot be cancelled, so it is left to the exiting process.
func confirm(cmd *cobra.Command, prompt string) error {
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		answer <- line
	}()
	select {
	case <-cmd.Context().Done():
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		return errInterrupted
	case line := <-answer:
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return nil
		}
		return errAborted
	}
}
