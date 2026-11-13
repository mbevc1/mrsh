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
	rotate    bool
	warned    bool // the --pass warning was given; bulk edits apply flags per host

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

// registerRotate adds --rotate-pass, for the commands that edit an existing entry.
func (f *secretFlags) registerRotate(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.rotate, "rotate-pass", false, "generate a new password, set it on the RouterOS devices that use it, and keep it where the old one was")
	cmd.MarkFlagsMutuallyExclusive("rotate-pass", "pass", "pass-env", "pass-arn")
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
		if name == "pass" && !f.warned {
			f.warned = true
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
	names      []string // hosts update: several --name values
	inGroups   []string // hosts update: --in-group
	port       int
	credential string
}

// register adds the host flags; with many, --name takes several hosts and
// --in-group selects whole groups (hosts update).
func (f *hostFlags) register(cmd *cobra.Command, opts *globalOptions, many bool) {
	fl := cmd.Flags()
	if many {
		fl.StringSliceVar(&f.names, "name", nil, "host to update; repeat or comma-separate for several")
		fl.StringSliceVar(&f.inGroups, "in-group", nil, "update every host in this group; repeat or comma-separate for several")
		_ = cmd.RegisterFlagCompletionFunc("in-group", completeGroups(opts))
	} else {
		fl.StringVar(&f.name, "name", "", "host name (unique key)")
		_ = cmd.MarkFlagRequired("name")
	}
	fl.IntVar(&f.port, "port", 0, "SSH port (0 = use defaults)")
	fl.StringVar(&f.credential, "credential", "", "use this credential set; its user, pass and key apply where the host sets none (\"\" to clear)")
	f.secretFlags.register(cmd)
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
	f.register(cmd, opts, false)
	return cmd
}

func newHostsUpdateCmd(opts *globalOptions) *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:     "update {--name NAME[,NAME...] | --in-group GROUP} [flags]",
		Aliases: []string{"u", "up"},
		Short:   "Update fields of one or more host entries (only the flags given)",
		Long: "Update fields of the hosts named by --name, or of every host in --in-group; only the flags\n" +
			"given change, on each of them. A new user or pass variant replaces the old one.\n" +
			"Several hosts are listed for confirmation first (unless -y). -H, --pass-ssm/--user-ssm\n" +
			"and --rotate-pass apply to one host at a time.\n\n" + sharedFlagsHelp + rotateFlagsHelp,
		Example: "  mrsh hosts update --name web03 -g db --pass-arn arn:aws:ssm:eu-west-1:123456789012:parameter/web03\n" +
			"  mrsh hosts update --name rb01,rb02 --credential routers\n" +
			"  mrsh hosts update --in-group lab -g staging --port 2222",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case len(f.names) == 0 && len(f.inGroups) == 0:
				return errors.New("hosts update needs --name NAME (repeat for several) or --in-group GROUP")
			case len(f.names) != 1 || len(f.inGroups) > 0:
				return f.updateMany(cmd, opts)
			}
			f.name = f.names[0]
			if f.rotate {
				if err := checkRotateOnly(cmd); err != nil {
					return err
				}
				lc, err := loadConfig(cmd, opts)
				if err != nil {
					return err
				}
				r, err := hostRotation(lc.cfg, f.name)
				if err != nil {
					return err
				}
				return r.rotate(cmd, opts, lc)
			}
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
	f.register(cmd, opts, true)
	f.registerRotate(cmd)
	_ = cmd.RegisterFlagCompletionFunc("name", completeNames(opts))
	return cmd
}

// updateMany applies the same field flags to several hosts in one config
// write, after listing them for confirmation.
func (f *hostFlags) updateMany(cmd *cobra.Command, opts *globalOptions) error {
	fl := cmd.Flags()
	switch {
	case f.rotate:
		return errors.New("--rotate-pass works on one host at a time; hosts that share a password rotate through their credential set")
	case f.wantsSSM(cmd):
		return errors.New("--pass-ssm/--user-ssm store one host's secret; for several hosts, store it in a credential set (credentials set --pass-ssm) and point them at it with --credential")
	case fl.Changed("host"):
		return errors.New("-H/--host sets one host's address; update several hosts' other fields only")
	}
	if err := f.store(cmd, opts, nil, "", ""); err != nil { // refuses a stray --overwrite
		return err
	}
	if changed, err := f.apply(cmd, opts, &config.Host{}); err != nil {
		return err
	} else if !changed {
		return errors.New("nothing to update: pass at least one field flag")
	}
	store, err := openStore(opts)
	if err != nil {
		return err
	}
	asked := false
	var names []string
	err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
		var err error
		if names, err = selectHosts(cfg, f.names, f.inGroups); err != nil {
			return err
		}
		if !opts.yes && !asked {
			asked = true
			if err := confirmHosts(cmd, fmt.Sprintf("About to update %d host(s)", len(names)), cfg, names); err != nil {
				return err
			}
		}
		for _, name := range names {
			h, _ := findHost(cfg, name)
			if _, err := f.apply(cmd, opts, &h); err != nil {
				return err
			}
			if err := d.ReplaceHost(name, h); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated %d host(s) in %s: %s\n", len(names), store.Location(), strings.Join(names, ", "))
	return err
}

// selectHosts resolves --name values and groups to host names, in config
// order without repeats. An unknown name is an error, as is a group with
// no hosts.
func selectHosts(cfg *config.Config, names, groups []string) ([]string, error) {
	want := map[string]bool{}
	for _, n := range names {
		if _, ok := findHost(cfg, n); !ok {
			return nil, fmt.Errorf("%w: %s", config.ErrHostNotFound, n)
		}
		want[n] = true
	}
	for _, g := range groups {
		found := false
		for _, h := range cfg.Hosts {
			if h.Group == g {
				want[h.Name], found = true, true
			}
		}
		if !found {
			return nil, fmt.Errorf("no hosts in group %s", g)
		}
	}
	var out []string
	for _, h := range cfg.Hosts {
		if want[h.Name] {
			out = append(out, h.Name)
		}
	}
	return out, nil
}

// confirmHosts lists hosts on stderr under intro and asks before acting.
func confirmHosts(cmd *cobra.Command, intro string, cfg *config.Config, names []string) error {
	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintln(w, intro+":")
	for _, n := range names {
		h, _ := findHost(cfg, n)
		_, _ = fmt.Fprintf(w, "  %s (%s)\n", h.Name, h.Host)
	}
	return confirm(cmd, "Continue? [y/N]: ")
}

func newHostsRemoveCmd(opts *globalOptions) *cobra.Command {
	var names []string
	var deleteSecret bool
	cmd := &cobra.Command{
		Use:     "remove {--name NAME[,NAME...] | -g GROUP}",
		Aliases: []string{"r", "rm"},
		Short:   "Remove one or more host entries (asks for confirmation unless -y/--assume-yes)",
		Long: "Remove the hosts named by --name (repeat or comma-separate for several), or every host in\n" +
			"the group given with -g, or both.",
		Example: "  mrsh hosts remove --name rb02\n  mrsh hosts remove --name rb02,rb03 --delete-secret\n  mrsh -g lab hosts remove",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var groups []string
			if cmd.Flags().Changed("group") {
				groups = []string{opts.group}
			}
			if len(names) == 0 && len(groups) == 0 {
				return errors.New("hosts remove needs --name NAME (repeat for several) or -g GROUP")
			}
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			asked := false
			var cfgAfter *config.Config
			var del, kept, gone []string
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				var err error
				if gone, err = selectHosts(cfg, names, groups); err != nil {
					return err
				}
				del, kept, cfgAfter = nil, nil, cfg
				also := ""
				if deleteSecret {
					del, kept = cfg.HostSecretDeletes(gone)
					if len(del) > 0 {
						also = fmt.Sprintf(" and delete %d SSM parameter(s)", len(del))
					}
				}
				if !opts.yes && !asked {
					asked = true
					if len(gone) == 1 && len(groups) == 0 {
						h, _ := findHost(cfg, gone[0])
						err = confirm(cmd, fmt.Sprintf("Remove host %s (%s) from %s%s? [y/N]: ", h.Name, h.Host, store.Location(), also))
					} else {
						err = confirmHosts(cmd, fmt.Sprintf("About to remove %d host(s) from %s%s", len(gone), store.Location(), also), cfg, gone)
					}
					if err != nil {
						return err
					}
				}
				for _, n := range gone {
					if err := d.RemoveHost(n); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			what := "host " + gone[0]
			if len(gone) > 1 {
				what = fmt.Sprintf("%d hosts (%s)", len(gone), strings.Join(gone, ", "))
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", what, store.Location()); err != nil {
				return err
			}
			return deleteSecrets(cmd, cfgAfter, del, kept)
		},
	}
	cmd.Flags().StringSliceVar(&names, "name", nil, "host to remove; repeat or comma-separate for several")
	cmd.Flags().BoolVar(&deleteSecret, "delete-secret", false, "also delete the hosts' own SSM parameters under ssm_prefix that nothing else uses")
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
