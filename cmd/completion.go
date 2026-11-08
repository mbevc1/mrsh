package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion bash|zsh|fish|powershell",
		Short: "Generate a shell completion script",
		Long: `Generate a shell completion script. Host names, groups and --name values
complete from the config given by -f (default hosts.yaml).

  bash:       source <(mrsh completion bash)
  zsh:        mrsh completion zsh > "${fpath[1]}/_mrsh"
  fish:       mrsh completion fish > ~/.config/fish/completions/mrsh.fish
  powershell: mrsh completion powershell | Out-String | Invoke-Expression`,
		Args:                  cobra.ExactArgs(1),
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, out := cmd.Root(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			}
			return fmt.Errorf("unsupported shell %q: use bash, zsh, fish or powershell", args[0])
		},
	}
}

// completionConfig loads the config for completions, quietly: a missing or
// invalid file just yields no suggestions.
func completionConfig(cmd *cobra.Command, opts *globalOptions) *config.Config {
	store, err := openStore(opts)
	if err != nil {
		return nil
	}
	raw, _, err := store.Load(cmd.Context())
	if err != nil {
		return nil
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil
	}
	return cfg
}

func completeHostNames(opts *globalOptions) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg := completionConfig(cmd, opts)
		if cfg == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		// --host takes comma-separated values; complete the last one.
		prefix, last := "", toComplete
		if i := strings.LastIndex(toComplete, ","); i >= 0 {
			prefix, last = toComplete[:i+1], toComplete[i+1:]
		}
		var out []string
		for _, h := range cfg.Hosts {
			if strings.HasPrefix(h.Name, last) {
				out = append(out, prefix+h.Name+"\t"+h.Host)
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}

func completeGroups(opts *globalOptions) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg := completionConfig(cmd, opts)
		if cfg == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		seen := map[string]bool{}
		var out []string
		for _, h := range cfg.Hosts {
			if h.Group != "" && !seen[h.Group] && strings.HasPrefix(h.Group, toComplete) {
				seen[h.Group] = true
				out = append(out, h.Group)
			}
		}
		sort.Strings(out)
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeNames completes an existing host's --name (hosts update/remove).
func completeNames(opts *globalOptions) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		names, dir := completeHostNames(opts)(cmd, args, toComplete)
		return names, dir &^ cobra.ShellCompDirectiveNoSpace
	}
}

func fixedValues(values ...string) cobra.CompletionFunc {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}
