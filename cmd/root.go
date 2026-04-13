package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/mbevc1/mrsh/pkg/config"
	"github.com/spf13/cobra"
)

// Global flags — set by cobra, read by subcommands.
var (
	cfgFile   string
	groupFlag string
	hostFlag  string
	hostsFlag string
	parallel  int
	timeout   int
	outputFmt string
	dryRun    bool

	// cfg is loaded once by PersistentPreRunE and shared across subcommands.
	cfg *config.Config
)

// Version info injected from main.go.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
	BuiltBy = ""
)

// Result holds the outcome of running a command on a single host.
type Result struct {
	Host       string `json:"host"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

var rootCmd = &cobra.Command{
	Use:   "mrsh",
	Short: "Multi Remote SHell — run commands on multiple hosts via SSH",
	Long: `mrsh runs shell commands over SSH on multiple hosts in parallel.

Hosts are defined in a YAML config file (default: hosts.yaml) and can be
targeted by group or individual name. Secrets may be stored as environment
variable references (env:VAR), AWS SSM parameters, or AWS Secrets Manager
secrets, and optionally encrypted at rest with SOPS.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		v, _ := cmd.Flags().GetBool("version")
		if v {
			fmt.Printf("%s version %s\n", Name, Version)
			if Commit != "" {
				fmt.Printf("  commit:   %s\n", Commit)
			}
			if Date != "" {
				fmt.Printf("  date:     %s\n", Date)
			}
			if BuiltBy != "" {
				fmt.Printf("  built by: %s\n", BuiltBy)
			}
			return nil
		}
		return cmd.Help()
	},
}

// Execute wires version info then runs the cobra command tree.
func Execute(v, c, d, b string) {
	Version = v
	Commit = c
	Date = d
	BuiltBy = b
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().BoolP("version", "v", false, "Print version information and exit")

	f := rootCmd.PersistentFlags()
	f.StringVarP(&cfgFile, "config", "f", "hosts.yaml", "Config file path")
	f.StringVarP(&groupFlag, "group", "g", "", "Filter hosts by group")
	f.StringVar(&hostFlag, "host", "", "Filter by host name or address")
	f.StringVar(&hostsFlag, "hosts", "", "Ad-hoc comma-separated hosts (user@host:port)")
	f.IntVarP(&parallel, "parallel", "p", 1, "Number of parallel SSH sessions")
	f.IntVarP(&timeout, "timeout", "t", 30, "SSH timeout in seconds")
	f.StringVar(&outputFmt, "output", "text", "Output format: text|json|csv")
	f.BoolVar(&dryRun, "dry-run", false, "Print what would run without executing")
}

// loadConfig loads the config file exactly once. Commands that operate on
// hosts must call this before calling filteredHosts(). Commands that don't
// need hosts (version, completion, hosts init) should NOT call it.
func loadConfig() error {
	if cfg != nil {
		return nil
	}
	var err error
	cfg, err = config.Load(cfgFile)
	if err != nil {
		return err
	}
	// CLI --timeout flag overrides config default.
	if cfg.Defaults.Port == 0 {
		cfg.Defaults.Port = 22
	}
	if timeout != 30 {
		cfg.Defaults.Timeout = timeout
	} else if cfg.Defaults.Timeout == 0 {
		cfg.Defaults.Timeout = timeout
	}
	return nil
}

// filteredHosts returns the hosts to operate on, applying all filters.
func filteredHosts() []config.Host {
	var adhoc []string
	if hostsFlag != "" {
		adhoc = strings.Split(hostsFlag, ",")
		for i := range adhoc {
			adhoc[i] = strings.TrimSpace(adhoc[i])
		}
	}
	return config.FilterHosts(cfg, groupFlag, hostFlag, adhoc)
}

// sshTimeout returns the connection timeout as a time.Duration.
func sshTimeout() time.Duration {
	t := timeout
	if cfg != nil && cfg.Defaults.Timeout > 0 {
		t = cfg.Defaults.Timeout
	}
	return time.Duration(t) * time.Second
}

// runParallel executes fn concurrently on each host using a semaphore of size n.
// Results are sorted by host address for deterministic output.
func runParallel(hosts []config.Host, n int, fn func(config.Host) Result) []Result {
	if n <= 0 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]Result, 0, len(hosts))

	for _, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(h config.Host) {
			defer wg.Done()
			defer func() { <-sem }()
			r := fn(h)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}(h)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].Host < results[j].Host
	})
	return results
}

// printResults writes results to stdout in the requested format.
func printResults(results []Result, format string) {
	switch format {
	case "json":
		printJSON(results)
	case "csv":
		printCSV(results)
	default:
		printText(results)
	}
}

func printText(results []Result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tNAME\tGROUP\tEXIT\tDURATION\tOUTPUT")
	for _, r := range results {
		output := r.Stdout
		if r.ExitCode != 0 && r.Stderr != "" {
			output = "[stderr] " + r.Stderr
		}
		// Collapse multi-line output to a single line for table display,
		// skipping blank lines produced by some devices (e.g. RouterOS).
		if strings.ContainsRune(output, '\n') {
			var parts []string
			for _, l := range strings.Split(output, "\n") {
				if s := strings.TrimSpace(l); s != "" {
					parts = append(parts, s)
				}
			}
			output = strings.Join(parts, " | ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dms\t%s\n",
			r.Host, r.Name, r.Group, r.ExitCode, r.DurationMs, output)
	}
	w.Flush()
}

func printJSON(results []Result) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		fmt.Fprintf(os.Stderr, "json encode error: %v\n", err)
	}
}

func printCSV(results []Result) {
	w := csv.NewWriter(os.Stdout)
	_ = w.Write([]string{"host", "name", "group", "exit_code", "stdout", "stderr", "duration_ms"})
	for _, r := range results {
		_ = w.Write([]string{
			r.Host,
			r.Name,
			r.Group,
			strconv.Itoa(r.ExitCode),
			r.Stdout,
			r.Stderr,
			strconv.FormatInt(r.DurationMs, 10),
		})
	}
	w.Flush()
}
