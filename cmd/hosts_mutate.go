package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

// sharedFlagsHelp explains the reused global flags in add/update help.
const sharedFlagsHelp = "The address, group, user and identity file come from the global flags:\n" +
	"  -H/--host ADDRESS, -g/--group GROUP, -u/--user USER, -i/--identity-file PATH.\n" +
	"Set at most one of --user/--user-env/--user-arn and of --pass/--pass-env/--pass-arn."

// hostFlags are the local flags of hosts add/update. The address, group,
// user and identity file reuse the global -H, -g, -u and -i flags, which
// have no other meaning for these commands.
type hostFlags struct {
	name    string
	port    int
	userEnv string
	userARN string
	pass    string
	passEnv string
	passARN string
}

func (f *hostFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "host name (unique key)")
	fl.IntVar(&f.port, "port", 0, "SSH port (0 = use defaults)")
	fl.StringVar(&f.userEnv, "user-env", "", "read the user from this environment variable")
	fl.StringVar(&f.userARN, "user-arn", "", "read the user from this SSM or Secrets Manager ARN")
	fl.StringVar(&f.pass, "pass", "", "literal password (visible in shell history; prefer --pass-env or --pass-arn)")
	fl.StringVar(&f.passEnv, "pass-env", "", "read the password from this environment variable")
	fl.StringVar(&f.passARN, "pass-arn", "", "read the password from this SSM or Secrets Manager ARN")
	_ = cmd.MarkFlagRequired("name")
	cmd.MarkFlagsMutuallyExclusive("pass", "pass-env", "pass-arn")
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
	if fl.Changed("identity-file") {
		h.IdentityFile, changed = opts.identityFile, true
	}

	userFlags := map[string]string{"user": opts.user, "user-env": f.userEnv, "user-arn": f.userARN}
	if name, v, ok, err := oneOf(cmd, userFlags); err != nil {
		return false, err
	} else if ok {
		h.User, h.UserEnv, h.UserARN = "", "", ""
		switch name {
		case "user":
			h.User = v
		case "user-env":
			h.UserEnv = v
		case "user-arn":
			h.UserARN = v
		}
		changed = true
	}
	passFlags := map[string]string{"pass": f.pass, "pass-env": f.passEnv, "pass-arn": f.passARN}
	if name, v, ok, err := oneOf(cmd, passFlags); err != nil {
		return false, err
	} else if ok {
		h.Pass, h.PassEnv, h.PassARN = "", "", ""
		switch name {
		case "pass":
			h.Pass = v
		case "pass-env":
			h.PassEnv = v
		case "pass-arn":
			h.PassARN = v
		}
		changed = true
	}
	return changed, nil
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
		Short:   "Add a host entry",
		Long:    "Add a host entry.\n\n" + sharedFlagsHelp,
		Example: "  mrsh hosts add --name web03 -H 10.0.0.3 -g web -u deploy --pass-env WEB03_PASS",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("host") {
				return errors.New("hosts add needs the address: -H/--host ADDRESS")
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
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "added host %s to %s\n", h.Name, store.Location())
			return err
		},
	}
	f.register(cmd)
	return cmd
}

func newHostsUpdateCmd(opts *globalOptions) *cobra.Command {
	var f hostFlags
	cmd := &cobra.Command{
		Use:   "update --name NAME [flags]",
		Short: "Update fields of a host entry (only the flags given)",
		Long: "Update fields of the host named by --name; only the flags given change.\n" +
			"A new user or pass variant replaces the old one.\n\n" + sharedFlagsHelp,
		Example: "  mrsh hosts update --name web03 -g db --pass-arn arn:aws:ssm:eu-west-1:123456789012:parameter/web03",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated host %s in %s\n", f.name, store.Location())
			return err
		},
	}
	f.register(cmd)
	return cmd
}

func newHostsRemoveCmd(opts *globalOptions) *cobra.Command {
	var name string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "remove --name NAME",
		Short: "Remove a host entry (asks for confirmation unless --confirm)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			asked := false
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				h, ok := findHost(cfg, name)
				if !ok {
					return fmt.Errorf("%w: %s", config.ErrHostNotFound, name)
				}
				if !confirm && !asked {
					asked = true
					if !askYesNo(cmd, fmt.Sprintf("Remove host %s (%s) from %s? [y/N]: ", h.Name, h.Host, store.Location())) {
						return errors.New("aborted")
					}
				}
				return d.RemoveHost(name)
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed host %s from %s\n", name, store.Location())
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "host name to remove")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "skip the confirmation prompt")
	_ = cmd.MarkFlagRequired("name")
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

// askYesNo prompts on stderr and reads one line from stdin; anything but
// y/yes (including EOF from a non-interactive stdin) means no.
func askYesNo(cmd *cobra.Command, prompt string) bool {
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
