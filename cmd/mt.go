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

All mt subcommands connect over standard SSH (port 22).`,
}

// ---- mt version ----

var mtVersionCmd = &cobra.Command{
	Use:     "version",
	Aliases: []string{"ve", "v"},
	Short:   "Show RouterOS firmware and package versions",
	RunE:    runMTVersion,
}

// ---- mt reboot ----

var mtRebootConfirm bool

var mtRebootCmd = &cobra.Command{
	Use:     "reboot",
	Aliases: []string{"re", "r"},
	Short:   "Reboot MikroTik device(s)",
	RunE:    runMTReboot,
}

// ---- mt upgrade ----

var mtUpgradeConfirm bool

var mtUpgradeCmd = &cobra.Command{
	Use:     "upgrade",
	Aliases: []string{"up", "u"},
	Short:   "Check for and install RouterOS package updates",
	RunE:    runMTUpgrade,
}

// ---- mt backup ----

var (
	mtBackupFormat string
	mtBackupPath   string
)

var mtBackupCmd = &cobra.Command{
	Use:     "backup",
	Aliases: []string{"ba", "b"},
	Short:   "Export and download RouterOS configuration backup",
	RunE:    runMTBackup,
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

// sshClient creates a plain SSH client for RouterOS.
func sshClient(h config.Host) *mrshshsh.Client {
	return &mrshshsh.Client{
		Host:    h.Address,
		User:    h.User,
		Pass:    h.Pass,
		KeyFile: h.KeyFile,
		Port:    h.Port,
		Timeout: sshTimeout(),
		Debug:   debugf,
	}
}

// ---- mt version ----

func runMTVersion(cmd *cobra.Command, args []string) error {
	if err := loadConfig(); err != nil {
		return err
	}
	hosts := filteredHosts()
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts matched")
	}

	results := runParallel(hosts, parallel, func(h config.Host) Result {
		r := Result{Host: h.Address, Name: h.Name, Group: h.Group}
		c := sshClient(h)
		defer c.Close()

		start := time.Now()
		resOut, _, _, err := c.Run("/system resource print")
		r.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			return r
		}

		pkgOut, _, _, _ := c.Run("/system package print")
		r.Stdout = resOut + "\n---\n" + pkgOut
		return r
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, colorHeader.Sprint("HOST\tNAME\tROUTEROS\tPACKAGES"))
	for _, r := range results {
		if r.ExitCode != 0 {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Name, colorFail.Sprint("ERROR"), r.Stderr)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Name,
			parseROSVersion(r.Stdout), parsePackageList(r.Stdout))
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
	}
	return "unknown"
}

func parsePackageList(output string) string {
	var packages []string
	inPkgSection := false
	nameOffset := -1

	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "---") {
			inPkgSection = true
			continue
		}
		if !inPkgSection {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		// Header row: use character offset of NAME for aligned extraction.
		if fields[0] == "#" {
			if idx := strings.Index(line, "NAME"); idx >= 0 {
				nameOffset = idx
			}
			continue
		}

		if !isNumeric(fields[0]) || len(fields) < 2 {
			continue
		}

		if nameOffset > 0 && len(line) > nameOffset {
			// Character-aligned extraction handles flag columns cleanly.
			if parts := strings.Fields(line[nameOffset:]); len(parts) > 0 {
				packages = append(packages, parts[0])
			}
		} else if isROSFlags(fields[1]) && len(fields) >= 3 {
			// Heuristic fallback: flag token before name (e.g. "2 XA routeros 7.22").
			packages = append(packages, fields[2])
		} else {
			packages = append(packages, fields[1])
		}
	}

	if len(packages) == 0 {
		return "n/a"
	}
	return strings.Join(packages, ", ")
}

// isROSFlags reports whether s looks like a RouterOS flags string (e.g. "X", "XA").
func isROSFlags(s string) bool {
	if len(s) == 0 || len(s) > 4 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
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
		c := sshClient(h)
		defer c.Close()

		start := time.Now()
		// No PTY — RouterOS skips the confirmation prompt in non-interactive
		// sessions and reboots immediately, dropping the connection.
		_, _, _, err := c.Run("/system reboot")
		r.DurationMs = time.Since(start).Milliseconds()
		if err != nil && !isConnectionReset(err) {
			r.ExitCode = 1
			r.Stderr = err.Error()
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
		c := sshClient(h)
		defer c.Close()

		start := time.Now()

		checkOut, _, _, err := c.Run("/system package update check-for-updates")
		if err != nil {
			r.ExitCode = 1
			r.Stderr = fmt.Sprintf("check-for-updates: %v", err)
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}

		if strings.Contains(checkOut, "available") || strings.Contains(checkOut, "new") {
			_, _, _, err = c.Run("/system package update install")
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
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "without exit status")
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
		c := sshClient(h)
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
	_, _, _, err := c.Run(fmt.Sprintf("/export compact file=%s", name))
	if err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	local := filepath.Join(localDir, name+".rsc")
	if err := c.Download(name+".rsc", local); err != nil {
		return "", fmt.Errorf("download rsc: %w", err)
	}
	_, _, _, _ = c.Run(fmt.Sprintf("/file remove %s.rsc", name))
	return fmt.Sprintf("RSC saved to %s", local), nil
}

func doBinaryBackup(c *mrshshsh.Client, h config.Host, name, localDir string) (string, error) {
	_, _, _, err := c.Run(fmt.Sprintf("/system backup save name=%s", name))
	if err != nil {
		return "", fmt.Errorf("backup save: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	local := filepath.Join(localDir, name+".backup")
	if err := c.Download(name+".backup", local); err != nil {
		return "", fmt.Errorf("download backup: %w", err)
	}
	_, _, _, _ = c.Run(fmt.Sprintf("/file remove %s.backup", name))
	return fmt.Sprintf("backup saved to %s", local), nil
}
