package cmd

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	routeros "github.com/go-routeros/routeros/v3"
	"github.com/mbevc1/mrsh/pkg/config"
	mrshshsh "github.com/mbevc1/mrsh/pkg/ssh"
	"github.com/spf13/cobra"
)

var mtCmd = &cobra.Command{
	Use:   "mt",
	Short: "MikroTik RouterOS automation shortcuts",
	Long: `mt provides shortcut subcommands for common MikroTik RouterOS operations
such as backup, reboot, package upgrade, and version reporting.

Subcommands connect via the RouterOS API (port 8728 plain by default).
To use TLS (port 8729) you must first assign a certificate on the device:
  /ip service set api-ssl certificate=<cert-name>
Then pass: --api-port 8729 --api-tls`,
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

// ---- API connection flags ----

var (
	mtAPIPort int
	mtAPITLS  bool
)

func init() {
	rootCmd.AddCommand(mtCmd)
	mtCmd.AddCommand(mtVersionCmd)
	mtCmd.AddCommand(mtRebootCmd)
	mtCmd.AddCommand(mtUpgradeCmd)
	mtCmd.AddCommand(mtBackupCmd)

	mtCmd.PersistentFlags().IntVar(&mtAPIPort, "api-port", 8728, "RouterOS API port (8728=plain, 8729=TLS)")
	mtCmd.PersistentFlags().BoolVar(&mtAPITLS, "api-tls", false, "Use TLS for RouterOS API (requires certificate on device)")

	mtRebootCmd.Flags().BoolVar(&mtRebootConfirm, "confirm", false, "Skip confirmation prompt")
	mtUpgradeCmd.Flags().BoolVar(&mtUpgradeConfirm, "confirm", false, "Skip confirmation prompt")
	mtBackupCmd.Flags().StringVar(&mtBackupFormat, "format", "rsc", "Backup format: rsc|backup|both")
	mtBackupCmd.Flags().StringVar(&mtBackupPath, "path", "backups", "Local directory for backup files")
}

// rosClient opens a RouterOS API connection to h.
func rosClient(h config.Host) (*routeros.Client, error) {
	addr := net.JoinHostPort(h.Address, fmt.Sprintf("%d", mtAPIPort))
	timeout := sshTimeout()
	if mtAPITLS {
		return routeros.DialTLSTimeout(addr, h.User, h.Pass, &tls.Config{
			InsecureSkipVerify: true, // RouterOS ships with self-signed certs by default
		}, timeout)
	}
	return routeros.DialTimeout(addr, h.User, h.Pass, timeout)
}

// sshClient creates a plain SSH client used by backup (file download still needs SSH).
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
		start := time.Now()

		c, err := rosClient(h)
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}
		defer c.Close()

		resReply, err := c.Run("/system/resource/print")
		r.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			return r
		}

		rosVer := "unknown"
		if len(resReply.Re) > 0 {
			if v := resReply.Re[0].Map["version"]; v != "" {
				rosVer = v
			}
		}

		pkgReply, _ := c.Run("/system/package/print")
		var pkgs []string
		for _, re := range pkgReply.Re {
			if name := re.Map["name"]; name != "" {
				pkgs = append(pkgs, name)
			}
		}

		pkgStr := "n/a"
		if len(pkgs) > 0 {
			pkgStr = strings.Join(pkgs, ", ")
		}
		r.Stdout = rosVer + "\x00" + pkgStr
		return r
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, colorHeader.Sprint("HOST\tNAME\tROUTEROS\tPACKAGES"))
	for _, r := range results {
		if r.ExitCode != 0 {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Name, colorFail.Sprint("ERROR"), r.Stderr)
			continue
		}
		parts := strings.SplitN(r.Stdout, "\x00", 2)
		rosVer, pkgs := "unknown", "n/a"
		if len(parts) > 0 {
			rosVer = parts[0]
		}
		if len(parts) > 1 {
			pkgs = parts[1]
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Name, rosVer, pkgs)
	}
	return w.Flush()
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
		c, err := rosClient(h)
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			return r
		}
		defer c.Close()

		start := time.Now()
		_, err = c.Run("/system/reboot")
		r.DurationMs = time.Since(start).Milliseconds()
		// RouterOS drops the connection immediately on reboot — that's expected.
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
		c, err := rosClient(h)
		if err != nil {
			r.ExitCode = 1
			r.Stderr = err.Error()
			return r
		}
		defer c.Close()

		start := time.Now()

		checkReply, err := c.Run("/system/package/update/check-for-updates")
		if err != nil {
			r.ExitCode = 1
			r.Stderr = fmt.Sprintf("check-for-updates: %v", err)
			r.DurationMs = time.Since(start).Milliseconds()
			return r
		}

		hasUpdate := false
		for _, re := range checkReply.Re {
			latest := re.Map["latest-version"]
			installed := re.Map["installed-version"]
			if (latest != "" && latest != installed) ||
				strings.Contains(strings.ToLower(re.Map["status"]), "new") {
				hasUpdate = true
				break
			}
		}

		if hasUpdate {
			_, err = c.Run("/system/package/update/install")
			// RouterOS reboots immediately after install — connection drop is expected.
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
// Backup still uses SSH: the RouterOS API has no file-download mechanism,
// so we need SSH to transfer the exported/saved files.

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
