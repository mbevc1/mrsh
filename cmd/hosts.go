package cmd

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

// starterConfig is written by `hosts init`.
const starterConfig = `# mrsh hosts config
defaults:
  port: 22
  timeout: 30
  parallel: 5
  output: text
  host_key_policy: insecure   # strict | accept-new | insecure
  # user: deploy
  # identity_file: ~/.ssh/id_ed25519
  # known_hosts: ~/.ssh/known_hosts

# Commands to run when 'mrsh run' gets no -c/--script.
# commands:
#   - uptime

hosts: []
# Set at most one of pass, pass_env or pass_arn per host (same for user):
#  - name: web01
#    host: 10.0.0.1
#    group: web
#    pass_env: WEB01_PASS
`

func newHostsCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "Manage the hosts config",
	}
	cmd.AddCommand(newHostsInitCmd(opts), newHostsListCmd(opts),
		newHostsAddCmd(opts), newHostsUpdateCmd(opts), newHostsRemoveCmd(opts))
	return cmd
}

func newHostsInitCmd(opts *globalOptions) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter hosts.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore(opts)
			if err != nil {
				return err
			}
			ifVersion := ""
			if force {
				_, ver, err := store.Load(cmd.Context())
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				ifVersion = ver
			}
			err = store.Save(cmd.Context(), []byte(starterConfig), ifVersion)
			if errors.Is(err, config.ErrVersionConflict) && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", store.Location())
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", store.Location())
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	return cmd
}

// hostRow is one `hosts list` entry, with secrets already masked or shown.
type hostRow struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Group        string `json:"group"`
	User         string `json:"user"`
	Pass         string `json:"pass"`
	IdentityFile string `json:"identity_file"`
}

func newHostsListCmd(opts *globalOptions) *cobra.Command {
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List hosts (secrets masked unless --show-secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lc, err := loadConfig(cmd, opts)
			if err != nil {
				return err
			}
			hosts, unmatched := lc.cfg.Select(opts.group, opts.hosts)
			for _, n := range unmatched {
				slog.Debug("--host not found in config", "host", n)
			}
			rows := make([]hostRow, 0, len(hosts))
			for _, h := range hosts {
				e := lc.cfg.Effective(h)
				rows = append(rows, hostRow{
					Name: e.Name, Host: e.Host, Port: e.Port, Group: e.Group,
					User:         e.UserRef().Display(showSecrets),
					Pass:         e.PassRef().Display(showSecrets),
					IdentityFile: e.IdentityFile,
				})
			}
			return writeHostRows(cmd.OutOrStdout(), opts.output, rows)
		},
	}
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "reveal literal user/pass values (references are never resolved)")
	return cmd
}

func writeHostRows(w io.Writer, format string, rows []hostRow) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	case "text":
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tHOST\tPORT\tGROUP\tUSER\tPASS\tIDENTITY")
		for _, r := range rows {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Name, r.Host, strconv.Itoa(r.Port), dash(r.Group), dash(r.User), dash(r.Pass), dash(r.IdentityFile))
		}
		return tw.Flush()
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"name", "host", "port", "group", "user", "pass", "identity_file"})
		for _, r := range rows {
			_ = cw.Write([]string{r.Name, r.Host, strconv.Itoa(r.Port), r.Group, r.User, r.Pass, r.IdentityFile})
		}
		cw.Flush()
		return cw.Error()
	}
	return fmt.Errorf("unknown output format %q", format)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
