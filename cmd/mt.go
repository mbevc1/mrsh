package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mbevc1/mrsh/pkg/config"
	mrshshsh "github.com/mbevc1/mrsh/pkg/ssh"
	"github.com/spf13/cobra"
)

var mtCmd = &cobra.Command{
	Use:   "mt",
	Short: "MikroTik RouterOS automation shortcuts",
	Long: `mt provides shortcut subcommands for common MikroTik RouterOS operations
such as backup, reboot, package upgrade, and version reporting.

All mt subcommands use PTY mode automatically, as required by RouterOS SSH.`,
}

// ---- mt version ----

var mtVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show RouterOS firmware and package versions",
	RunE:  runMTVersion,
}

// ---- mt reboot ----

var (
	mtRebootConfirm bool
)

var mtRebootCmd = &cobra.Command{
	Use:   "reboot",
	Short: "Reboot MikroTik device(s)",
	RunE:  runMTReboot,
}

// ---- mt upgrade ----

var (
	mtUpgradeConfirm bool
)

var mtUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Check for and install RouterOS package updates",
	RunE:  runMTUpgrade,
}

// ---- mt backup ----

var (
	mtBackupFormat string
	mtBackupPath   string
)

var mtBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Export and download RouterOS configuration backup",
	RunE:  runMTBackup,
}

func init() {
	rootCmd.AddCommand(mtCmd)
	mtCmd.AddCommand(mtVersionCmd)
	mtCmd.AddCommand(mtRebootCmd)
	mtCmd.AddCommand(mtUpgradeCmd)
	mtCmd.AddCommand(mtBackupCmd)

	mtRebootCmd.Flags().BoolVar(&mtRebootConfirm, "confirm", false, "Skip confirmation prompt")
	mtUpgradeCmd.Flags().BoolVar(&mtUpgradeConfirm, "confirm", false, "Skip confirmation prompt")
	mtBackupCmd.Flags().StringVar(&mtBackupFormat, "format", "rsc", "Backup format: rsc|backup|both")
	mtBackupCmd.Flags().StringVar(&mtBackupPath, "path", "backups", "Local directory for backup files")
}

// mtClient creates an SSH client with PTY enabled for RouterOS.
func mtClient(h config.Host) *mrshshsh.Client {
	return &mrshshsh.Client{
		Host:       h.Address,
		User:       h.User,
		Pass:       h.Pass,
		KeyFile:    h.KeyFile,
		Port:       h.Port,
		Timeout:    sshTimeout(),
		RequestPTY: true,
	}
}

// ---- mt version ----

type mtVersionResult struct {
	Host       string
	Name       string
	RBFirmware string
	UpgradeFW  string
	ROSVersion string
	Packages   string
	Error      string
}

func runMTVersion(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched")
	}

	type versionResult struct {
		host   config.Host
		parsed mtVersionResult
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		r := Result{Host: h.Address, Name: h.Name, Group: h.Group}
		c := mtClient(h)
		defer c.Close()

		start := time.Now()
		rbOut, _, _, err := c.RunWithPTY(":put [/system routerboard get as-value]")
		r.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			return r
		}

		pkgOut, _, _, _ := c.RunWithPTY("/system package print")
		r.Stdout = rbOut + "\n---\n" + pkgOut
		return r
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tNAME\tROUTEROS\tPACKAGES")
	for _, r := range results {
		if r.ExitCode != 0 {
			fmt.Fprintf(w, "%s\t%s\tERROR\t%s\n", r.Host, r.Name, r.Stderr)
			continue
		}
		rosVer := parseROSVersion(r.Stdout)
		packages := parsePackageList(r.Stdout)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Name, rosVer, packages)
	}
	return w.Flush()
}

func parseROSVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "version:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
		// RouterOS ":put" output format: key=value
		if strings.HasPrefix(line, "version=") {
			return strings.TrimPrefix(line, "version=")
		}
	}
	return "unknown"
}

func parsePackageList(output string) string {
	var packages []string
	inPkgSection := false
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "---") {
			inPkgSection = true
			continue
		}
		if inPkgSection {
			line = strings.TrimSpace(line)
			// Package lines typically start with a number or contain "name="
			if strings.Contains(line, "name=") {
				for _, part := range strings.Fields(line) {
					if strings.HasPrefix(part, "name=") {
						packages = append(packages, strings.TrimPrefix(part, "name="))
					}
				}
			}
		}
	}
	if len(packages) == 0 {
		return "n/a"
	}
	return strings.Join(packages, ", ")
}

// ---- mt reboot ----

func runMTReboot(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched")
	}

	if !mtRebootConfirm {
		fmt.Printf("About to REBOOT %d host(s). This will interrupt all connections.\n", len(hosts))
		for _, h := range hosts {
			fmt.Printf("  %s (%s)\n", h.Address, h.Name)
		}
		r := bufio.NewReader(os.Stdin)
		answer := prompt(r, "Proceed? [y/N]", "N")
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		r := Result{Host: h.Address, Name: h.Name, Group: h.Group}
		c := mtClient(h)
		defer c.Close()

		start := time.Now()
		_, _, _, err := c.RunWithPTY("/system reboot")
		r.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			// RouterOS disconnects immediately on reboot; connection reset is expected.
			if strings.Contains(err.Error(), "connection reset") ||
				strings.Contains(err.Error(), "EOF") ||
				strings.Contains(err.Error(), "broken pipe") {
				r.Stdout = "reboot initiated"
			} else {
				r.ExitCode = 1
				r.Stderr = err.Error()
			}
		} else {
			r.Stdout = "reboot initiated"
		}
		return r
	})

	printResults(results, outputFmt)
	return nil
}

// ---- mt upgrade ----

func runMTUpgrade(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched")
	}

	if !mtUpgradeConfirm {
		fmt.Printf("About to upgrade packages on %d host(s).\n", len(hosts))
		fmt.Println("WARNING: Devices will reboot after upgrade.")
		for _, h := range hosts {
			fmt.Printf("  %s (%s)\n", h.Address, h.Name)
		}
		r := bufio.NewReader(os.Stdin)
		answer := prompt(r, "Proceed? [y/N]", "N")
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		r := Result{Host: h.Address, Name: h.Name, Group: h.Group}
		c := mtClient(h)
		defer c.Close()

		start := time.Now()

		// Step 1: check for updates
		checkOut, _, _, err := c.RunWithPTY("/system package update check-for-updates")
		if err != nil {
			r.ExitCode = 1
			r.Stderr = fmt.Sprintf("check-for-updates: %v", err)
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}

		// Step 2: install if updates available
		if strings.Contains(checkOut, "available") || strings.Contains(checkOut, "new") {
			_, _, _, err = c.RunWithPTY("/system package update install")
			// Device reboots on install, so connection drop is expected.
			if err != nil && !isConnectionReset(err) {
				r.ExitCode = 1
				r.Stderr = fmt.Sprintf("install: %v", err)
			} else {
				r.Stdout = "upgrade initiated, device rebooting"
			}
		} else {
			r.Stdout = "already up to date"
		}

		r.DurationMs = time.Since(start).Milliseconds()
		return r
	})

	printResults(results, outputFmt)
	return nil
}

func isConnectionReset(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe")
}

// ---- mt backup ----

func runMTBackup(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched")
	}

	if err := os.MkdirAll(mtBackupPath, 0750); err != nil {
		return fmt.Errorf("create backup dir %s: %w", mtBackupPath, err)
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		r := Result{Host: h.Address, Name: h.Name, Group: h.Group}
		c := mtClient(h)
		defer c.Close()

		start := time.Now()
		name := fmt.Sprintf("mrsh-backup-%s", h.Name)
		localDir := filepath.Join(mtBackupPath, h.Group, h.Name)
		if err := os.MkdirAll(localDir, 0750); err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}

		var msgs []string

		if mtBackupFormat == "rsc" || mtBackupFormat == "both" {
			if msg, err := doRSCBackup(c, h, name, localDir); err != nil {
				r.ExitCode = 1
				r.Stderr = err.Error()
				r.DurationMs = time.Since(start).Milliseconds()
				return r
			} else {
				msgs = append(msgs, msg)
			}
		}

		if mtBackupFormat == "backup" || mtBackupFormat == "both" {
			if msg, err := doBinaryBackup(c, h, name, localDir); err != nil {
				r.ExitCode = 1
				r.Stderr = err.Error()
				r.DurationMs = time.Since(start).Milliseconds()
				return r
			} else {
				msgs = append(msgs, msg)
			}
		}

		r.Stdout = strings.Join(msgs, "; ")
		r.DurationMs = time.Since(start).Milliseconds()
		return r
	})

	printResults(results, outputFmt)
	return nil
}

func doRSCBackup(c *mrshshsh.Client, h config.Host, name, localDir string) (string, error) {
	// Export RSC config on the device.
	_, _, _, err := c.RunWithPTY(fmt.Sprintf("/export compact file=%s", name))
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}

	// Small delay to allow file write to complete on device.
	time.Sleep(500 * time.Millisecond)

	local := filepath.Join(localDir, name+".rsc")
	if err := c.Download(name+".rsc", local); err != nil {
		return "", fmt.Errorf("download rsc: %w", err)
	}

	// Clean up remote file.
	_, _, _, _ = c.RunWithPTY(fmt.Sprintf("/file remove %s.rsc", name))

	return fmt.Sprintf("RSC saved to %s", local), nil
}

func doBinaryBackup(c *mrshshsh.Client, h config.Host, name, localDir string) (string, error) {
	// Create binary backup on device.
	_, _, _, err := c.RunWithPTY(fmt.Sprintf("/system backup save name=%s", name))
	if err != nil {
		return "", fmt.Errorf("backup save: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	local := filepath.Join(localDir, name+".backup")
	// NOTE: Binary backup files may not transfer correctly via `cat` due to
	// NUL bytes. Use SFTP for reliable binary transfer (future enhancement).
	if err := c.Download(name+".backup", local); err != nil {
		return "", fmt.Errorf("download backup: %w", err)
	}

	_, _, _, _ = c.RunWithPTY(fmt.Sprintf("/file remove %s.backup", name))

	return fmt.Sprintf("backup saved to %s", local), nil
}
