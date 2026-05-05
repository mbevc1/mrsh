package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/mbevc1/mrsh/pkg/config"
	"github.com/spf13/cobra"
)

// Global flags — set by cobra, read by subcommands.
var (
	cfgFile   string
	groupFlag string
	nameFlag  string
	hostFlag  string
	hostsFlag string
	parallel  int
	timeout   int
	outputFmt string
	dryRun    bool
	debugFlag bool

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
	f.StringVarP(&nameFlag, "name", "n", "", "Filter hosts by name")
	f.StringVar(&hostFlag, "host", "", "Filter by host address")
	f.StringVar(&hostsFlag, "hosts", "", "Ad-hoc comma-separated hosts (user@host:port)")
	f.IntVarP(&parallel, "parallel", "p", 1, "Number of parallel SSH sessions")
	f.IntVarP(&timeout, "timeout", "t", 30, "SSH timeout in seconds")
	f.StringVarP(&outputFmt, "output", "o", "text", "Output format: text|json|csv")
	f.BoolVar(&dryRun, "dry-run", false, "Print what would run without executing")
	f.BoolVar(&debugFlag, "debug", false, "Print verbose diagnostic output to stderr")
}

// debugf writes a diagnostic message to stderr when --debug is set.
func debugf(format string, args ...interface{}) {
	if !debugFlag {
		return
	}
	colorDebug.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
}

// loadConfig loads the config file exactly once. Commands that operate on
// hosts must call this before calling filteredHosts(). Commands that don't
// need hosts (version, completion, hosts init) should NOT call it.
func loadConfig() error {
	if cfg != nil {
		return nil
	}
	debugf("loading config: %s", cfgFile)
	var err error
	cfg, err = config.Load(cfgFile)
	if err != nil {
		return err
	}
	debugf("loaded %d host(s) from config", len(cfg.Hosts))
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
	hosts := config.FilterHosts(cfg, groupFlag, hostFlag, adhoc)
	if nameFlag != "" {
		filtered := hosts[:0]
		for _, h := range hosts {
			if h.Name == nameFlag {
				filtered = append(filtered, h)
			}
		}
		hosts = filtered
	}
	debugf("filters: group=%q name=%q host=%q adhoc=%v -> %d host(s) matched",
		groupFlag, nameFlag, hostFlag, adhoc, len(hosts))
	for _, h := range hosts {
		debugf("  target: name=%s addr=%s group=%s user=%s port=%d",
			h.Name, h.Address, h.Group, h.User, h.Port)
	}
	return hosts
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
	debugf("running on %d host(s) with parallelism=%d", len(hosts), n)
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

var (
	colorHeader = color.New(color.Bold)
	colorHost   = color.New(color.FgCyan, color.Bold)
	colorOK     = color.New(color.FgGreen)
	colorFail   = color.New(color.FgRed)
	colorDebug  = color.New(color.FgYellow)
)

// ansiEsc matches ANSI SGR escape sequences so we can measure visible width.
var ansiEsc = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visLen returns the visible character width of s, stripping ANSI escape codes.
func visLen(s string) int {
	return len(ansiEsc.ReplaceAllString(s, ""))
}

// printTable prints a column-aligned table with a bold header row.
// Cells may contain ANSI escape codes; column widths are derived from visible
// characters only, so coloured cells don't skew alignment.
func printTable(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if w := visLen(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	pr := func(cells []string) {
		for i, cell := range cells {
			fmt.Print(cell)
			if i < len(cells)-1 {
				fmt.Print(strings.Repeat(" ", widths[i]-visLen(cell)+2))
			}
		}
		fmt.Println()
	}
	styledHdrs := make([]string, len(headers))
	for i, h := range headers {
		styledHdrs[i] = colorHeader.Sprint(h)
	}
	pr(styledHdrs)
	for _, row := range rows {
		pr(row)
	}
}

func printText(results []Result) {
	rows := make([][]string, len(results))
	for i, r := range results {
		exit := colorOK.Sprintf("%d", r.ExitCode)
		if r.ExitCode != 0 {
			exit = colorFail.Sprintf("%d", r.ExitCode)
		}
		rows[i] = []string{r.Host, r.Name, r.Group, exit, fmt.Sprintf("%dms", r.DurationMs)}
	}
	printTable([]string{"HOST", "NAME", "GROUP", "EXIT", "DURATION"}, rows)

	for _, r := range results {
		output := r.Stdout
		if r.ExitCode != 0 && r.Stderr != "" {
			output = r.Stderr
		}
		if output == "" {
			continue
		}
		fmt.Printf("\n[%s]\n%s\n", colorHost.Sprint(r.Host), output)
	}
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
