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
		Aliases: []string{"creds", "cred"},
		Short:   "Manage reusable credential sets (user, pass, identity file)",
		Long: "A credential set is a named user/pass/identity_file that hosts and defaults reference\n" +
			"with `credential: NAME` (or hosts add/update --credential NAME). For each of user, pass\n" +
			"and identity file, a host uses its own value, else its set's, else defaults', else the\n" +
			"set named in defaults.",
	}
	cmd.AddCommand(newCredentialsListCmd(opts), newCredentialsSetCmd(opts), newCredentialsRemoveCmd(opts))
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
