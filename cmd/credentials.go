package cmd

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

func newCredentialsCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "credentials",
		Aliases: []string{"creds", "cred", "identities", "identity", "ids"},
		Short:   "Manage reusable credential sets (user, pass, identity file)",
		Long: "A credential set is a named user/pass/identity_file that hosts and defaults reference\n" +
			"with `credential: NAME` (or hosts add/update --credential NAME). For each of user, pass\n" +
			"and identity file, a host uses its own value, else its set's, else defaults', else the\n" +
			"set named in defaults.",
	}
	cmd.AddCommand(newCredentialsListCmd(opts), newCredentialsSetCmd(opts), newCredentialsRemoveCmd(opts),
		newCredentialsDefaultCmd(opts), newCredentialsAssignCmd(opts), newCredentialsRenameCmd(opts))
	return cmd
}

// credentialRow is one `credentials list` entry, with passwords masked.
type credentialRow struct {
	Name         string   `json:"name"`
	User         string   `json:"user"`
	Pass         string   `json:"pass"`
	IdentityFile string   `json:"identity_file"`
	UsedBy       []string `json:"used_by"`
}

func newCredentialsListCmd(opts *globalOptions) *cobra.Command {
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List credential sets and who uses them (passwords masked unless --show-secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lc, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}
			rows := make([]credentialRow, 0, len(lc.cfg.Credentials))
			for _, name := range sortedNames(lc.cfg.Credentials) {
				set := lc.cfg.Credentials[name]
				used := lc.cfg.CredentialUsers(name)
				if used == nil {
					used = []string{}
				}
				rows = append(rows, credentialRow{
					Name: name, User: set.UserRef().Display(true), Pass: set.PassRef().Display(showSecrets),
					IdentityFile: set.IdentityFile, UsedBy: used,
				})
			}
			return writeCredentialRows(cmd.OutOrStdout(), opts.output, rows)
		},
	}
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "reveal literal passwords (references are never resolved)")
	return cmd
}

func writeCredentialRows(w io.Writer, format string, rows []credentialRow) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"name", "user", "pass", "identity_file", "used_by"})
		for _, r := range rows {
			_ = cw.Write([]string{r.Name, r.User, r.Pass, r.IdentityFile, strings.Join(r.UsedBy, " ")})
		}
		cw.Flush()
		return cw.Error()
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tUSER\tPASS\tIDENTITY\tUSED BY")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, dash(r.User), dash(r.Pass), dash(r.IdentityFile), dash(strings.Join(r.UsedBy, ", ")))
	}
	return tw.Flush()
}

func newCredentialsSetCmd(opts *globalOptions) *cobra.Command {
	var name string
	var f secretFlags
	cmd := &cobra.Command{
		Use:   "set --name NAME [flags]",
		Short: "Create a credential set, or change the given fields of an existing one",
		Long: "Create the named credential set, or update it: only the flags given change, and a new\n" +
			"user or pass variant replaces the old one. The user and identity file come from the\n" +
			"global -u/--user and -i/--identity-file (-i \"\" clears it).",
		Example: "  mrsh credentials set --name routers -u admin --pass-arn arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/routers\n" +
			"  mrsh hosts update --name rt1 --credential routers",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			created := false
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				set, exists := cfg.Credentials[name]
				created = !exists
				changed, err := f.apply(cmd, opts, &set.Credentials)
				if err != nil {
					return err
				}
				if cmd.Flags().Changed("identity-file") {
					set.IdentityFile, changed = opts.identityFile, true
				}
				if !changed {
					return errors.New("nothing to set: pass at least one of -u/--user-env/--user-arn, --pass/--pass-env/--pass-arn, -i")
				}
				return d.SetCredential(name, set)
			})
			if err != nil {
				return err
			}
			verb := "updated"
			if created {
				verb = "added"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s credential %s in %s\n", verb, name, store.Location())
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "credential set name")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.RegisterFlagCompletionFunc("name", completeCredentials(opts))
	f.register(cmd)
	return cmd
}

func newCredentialsRemoveCmd(opts *globalOptions) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "remove --name NAME",
		Short: "Remove an unused credential set (asks for confirmation unless -y/--assume-yes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			asked := false
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				if _, ok := cfg.Credentials[name]; !ok {
					return fmt.Errorf("%w: %s", config.ErrCredentialNotFound, name)
				}
				if used := cfg.CredentialUsers(name); len(used) > 0 {
					return fmt.Errorf("credential %s is used by %s; point them at another set first", name, strings.Join(used, ", "))
				}
				if !opts.yes && !asked {
					asked = true
					if !askYesNo(cmd, fmt.Sprintf("Remove credential %s from %s? [y/N]: ", name, store.Location())) {
						return errors.New("aborted")
					}
				}
				return d.RemoveCredential(name)
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed credential %s from %s\n", name, store.Location())
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "credential set name")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.RegisterFlagCompletionFunc("name", completeCredentials(opts))
	return cmd
}

func newCredentialsDefaultCmd(opts *globalOptions) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "default [NAME]",
		Short: "Show, set or --clear the credential set used by defaults",
		Long: "With no argument, print the set defaults use. With NAME, make every host that sets\n" +
			"no user, pass or identity file of its own (and has no set of its own) fall back to it.",
		Args: cobra.MaximumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return completeCredentials(opts)(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !clear {
				lc, err := loadConfig(cmd, opts)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), dash(lc.cfg.Defaults.Credential))
				return err
			}
			if len(args) == 1 && clear {
				return errors.New("give a NAME or --clear, not both")
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, _ *config.Config) error {
				return d.SetDefaultCredential(name)
			})
			if err != nil {
				return err
			}
			msg := fmt.Sprintf("defaults now use credential %s", name)
			if name == "" {
				msg = "defaults no longer use a credential set"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s in %s\n", msg, store.Location())
			return err
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "stop defaults using a credential set")
	return cmd
}

func newCredentialsAssignCmd(opts *globalOptions) *cobra.Command {
	var name string
	var clear bool
	cmd := &cobra.Command{
		Use:   "assign (--name NAME | --clear) {-g GROUP | -H HOST}...",
		Short: "Point many hosts at a credential set (or --clear it) by group or name",
		Long: "Set credential: NAME on every config host selected by the global -g/--group and\n" +
			"-H/--host flags (config names or addresses; literal addresses are refused). Each\n" +
			"host's own user, pass and identity file are left as they are, and still win.",
		Example: "  mrsh credentials assign --name routers -g routers\n" +
			"  mrsh credentials assign --clear -H rb01,rb02",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (name == "") == !clear {
				return errors.New("give --name NAME or --clear")
			}
			if opts.group == "" && len(opts.hosts) == 0 {
				return errors.New("select hosts with -g/--group and/or -H/--host")
			}
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			var changed []string
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				changed = changed[:0]
				hosts, unmatched := cfg.Select(opts.group, opts.hosts)
				if len(unmatched) > 0 {
					return fmt.Errorf("not in the config: %s", strings.Join(unmatched, ", "))
				}
				if len(hosts) == 0 {
					return errors.New("no hosts selected")
				}
				for _, h := range hosts {
					if h.Credential == name {
						continue
					}
					h.Credential = name
					if err := d.ReplaceHost(h.Name, h); err != nil {
						return err
					}
					changed = append(changed, h.Name)
				}
				return nil
			})
			if err != nil {
				return err
			}
			verb := "assigned credential " + name + " to"
			if clear {
				verb = "cleared the credential set on"
			}
			if len(changed) == 0 {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "nothing to change in %s\n", store.Location())
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %d host(s) in %s: %s\n", verb, len(changed), store.Location(), strings.Join(changed, ", "))
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "credential set to assign")
	cmd.Flags().BoolVar(&clear, "clear", false, "remove the credential set from the selected hosts")
	cmd.MarkFlagsMutuallyExclusive("name", "clear")
	_ = cmd.RegisterFlagCompletionFunc("name", completeCredentials(opts))
	return cmd
}

func newCredentialsRenameCmd(opts *globalOptions) *cobra.Command {
	var name, to string
	cmd := &cobra.Command{
		Use:   "rename --name OLD --to NEW",
		Short: "Rename a credential set and update every host and defaults that use it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			var moved []string
			err = config.Mutate(cmd.Context(), store, func(d *config.Document, cfg *config.Config) error {
				moved = cfg.CredentialUsers(name)
				set, ok := cfg.Credentials[name]
				if !ok {
					return fmt.Errorf("%w: %s", config.ErrCredentialNotFound, name)
				}
				if _, taken := cfg.Credentials[to]; taken {
					return fmt.Errorf("credential %s already exists", to)
				}
				if err := d.SetCredential(to, set); err != nil {
					return err
				}
				if err := d.RemoveCredential(name); err != nil {
					return err
				}
				if cfg.Defaults.Credential == name {
					if err := d.SetDefaultCredential(to); err != nil {
						return err
					}
				}
				for _, h := range cfg.Hosts {
					if h.Credential == name {
						h.Credential = to
						if err := d.ReplaceHost(h.Name, h); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			users := "no references"
			if len(moved) > 0 {
				users = "updated " + strings.Join(moved, ", ")
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "renamed credential %s to %s in %s (%s)\n", name, to, store.Location(), users)
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "current name")
	cmd.Flags().StringVar(&to, "to", "", "new name")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.RegisterFlagCompletionFunc("name", completeCredentials(opts))
	return cmd
}
