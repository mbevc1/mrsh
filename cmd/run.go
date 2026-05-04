package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/mbevc1/mrsh/pkg/config"
	mrshshsh "github.com/mbevc1/mrsh/pkg/ssh"
	"github.com/spf13/cobra"
)

var (
	runCommand string
	runScript  string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a command or script on remote hosts",
	Long: `Run executes a shell command (or a local script file) on all matching
remote hosts over SSH. Results are printed in the requested output format.

Examples:
  mrsh run -c "uptime"
  mrsh run -c "df -h" --group web --output json
  mrsh run --script deploy.sh --group app --parallel 5
  mrsh --hosts "admin@10.0.0.1,admin@10.0.0.2" run -c "hostname"`,
	RunE: runRunCmd,
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().StringVarP(&runCommand, "command", "c", "", "Command to run on remote hosts")
	runCmd.Flags().StringVar(&runScript, "script", "", "Local script file to execute remotely via bash -s")
}

func runRunCmd(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}

	// Determine what to execute.
	var scriptBytes []byte
	commandStr := runCommand

	if runScript != "" {
		var err error
		scriptBytes, err = os.ReadFile(runScript)
		if err != nil {
			return fmt.Errorf("reading script %s: %w", runScript, err)
		}
	} else if commandStr == "" && len(cfg.Commands) > 0 {
		// Fall back to commands defined in config.
		commandStr = cfg.Commands[0]
	} else if commandStr == "" {
		return fmt.Errorf("specify a command with -c or a script with --script")
	}

	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched the given filters")
	}

	if dryRun {
		printDryRun(hosts, commandStr, runScript)
		return nil
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		return executeOnHost(h, commandStr, scriptBytes)
	})

	printResults(results, outputFmt)
	return nil
}

// executeOnHost runs a command or script on a single host and returns a Result.
func executeOnHost(h config.Host, command string, script []byte) Result {
	start := time.Now()
	r := Result{
		Host:  h.Address,
		Name:  h.Name,
		Group: h.Group,
	}

	client := &mrshshsh.Client{
		Host:    h.Address,
		User:    h.User,
		Pass:    h.Pass,
		KeyFile: h.KeyFile,
		Port:    h.Port,
		Timeout: sshTimeout(),
		Debug:   debugf,
	}
	defer client.Close()

	var stdout, stderr string
	var exitCode int
	var err error

	if len(script) > 0 {
		stdout, stderr, exitCode, err = client.UploadScript(script)
	} else {
		stdout, stderr, exitCode, err = client.Run(command)
	}

	r.DurationMs = time.Since(start).Milliseconds()

	if err != nil {
		r.ExitCode = 1
		r.Stderr = err.Error()
		return r
	}

	r.Stdout = stdout
	r.Stderr = stderr
	r.ExitCode = exitCode
	return r
}

func printDryRun(hosts []config.Host, command, script string) {
	action := fmt.Sprintf("command: %q", command)
	if script != "" {
		action = fmt.Sprintf("script: %q", script)
	}
	fmt.Printf("[dry-run] Would execute %s on %d host(s):\n", action, len(hosts))
	for _, h := range hosts {
		fmt.Printf("  %s (%s) group=%s\n", h.Address, h.Name, h.Group)
	}
}
