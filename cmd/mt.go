package cmd

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/runner"
	"github.com/mbevc1/mrsh/internal/ssh"
	"github.com/mbevc1/mrsh/internal/storage"
)

// Timing knobs; tests shorten them.
var (
	now              = time.Now
	fileWaitTimeout  = 30 * time.Second
	fileWaitInterval = 500 * time.Millisecond
	updateWait       = 60 * time.Second
	updatePoll       = 2 * time.Second
)

func newMtCmd(opts *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mt",
		Short: "MikroTik RouterOS (v7) shortcuts",
	}
	cmd.AddCommand(newMtBackupCmd(opts), newMtRebootCmd(opts), newMtUpgradeCmd(opts), newMtVersionCmd(opts))
	return cmd
}

func mtTargets(cmd *cobra.Command, opts *globalOptions) ([]runner.Target, error) {
	lc, err := loadConfig(cmd, opts)
	if err != nil {
		return nil, err
	}
	return selectTargets(lc, opts)
}

func newMtBackupCmd(opts *globalOptions) *cobra.Command {
	var format, dest string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Export and download config backups",
		Long: "Writes backup-<Day>.rsc / .backup on each device (a rolling 7-day set kept on the device),\n" +
			"then downloads dated copies over SFTP to --path as <group>/<name>/<YYYY-MM-DD>.<ext>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exts, err := mt.Formats(format)
			if err != nil {
				return err
			}
			if err := opts.sse().Validate(); err != nil {
				return err
			}
			targets, err := mtTargets(cmd, opts)
			if err != nil {
				return err
			}
			sink, err := storage.NewSink(dest, storage.SinkOptions{SSE: opts.sseMode, KMSKey: opts.kmsKey})
			if err != nil {
				return err
			}
			day, date := mt.Weekday(now()), now()
			if opts.dryRun {
				var cmds []string
				for _, ext := range exts {
					cmds = append(cmds, backupCmd(ext, day))
				}
				return printDryRun(cmd.OutOrStdout(), targets, strings.Join(cmds, "\n"), nil)
			}
			ctx := cmd.Context()
			if err := sink.Preflight(ctx); err != nil {
				return fmt.Errorf("backup destination %s: %w", sink.Location(), err)
			}
			if err := prepareConnect(ctx, opts, targets); err != nil {
				return err
			}
			results := runner.Execute(ctx, targets, opts.parallel, runner.WithConn(connOptions(opts),
				func(ctx context.Context, conn *ssh.Conn, t runner.Target, res *runner.Result) {
					var saved []string
					for _, ext := range exts {
						key := mt.BackupKey(t.Group, t.Name, t.Literal, date, ext)
						n, err := backupOne(ctx, conn, day, ext, sink, key)
						if err != nil {
							res.Err, res.ExitCode = err, -1
							break
						}
						saved = append(saved, fmt.Sprintf("saved %s (%d bytes)", key, n))
					}
					res.Stdout = strings.Join(saved, "\n")
				}))
			if err := runner.Write(cmd.OutOrStdout(), opts.output, results); err != nil {
				return err
			}
			return outcome(ctx, results)
		},
	}
	cmd.Flags().StringVar(&format, "format", mt.FormatBoth, "rsc|backup|both")
	_ = cmd.RegisterFlagCompletionFunc("format", fixedValues(mt.FormatRSC, mt.FormatBackup, mt.FormatBoth))
	cmd.Flags().StringVar(&dest, "path", "backups/", "destination: local dir or s3://bucket/prefix/")
	return cmd
}

func backupCmd(ext, day string) string {
	if ext == "rsc" {
		return mt.ExportCmd(day)
	}
	return mt.SaveBackupCmd(day)
}

// backupOne writes one backup file on the device, waits for it, and streams
// it into the sink. The device copy stays as the rolling weekday backup.
func backupOne(ctx context.Context, conn *ssh.Conn, day, ext string, sink storage.Sink, key string) (int64, error) {
	stdout, stderr, code, err := conn.Run(ctx, backupCmd(ext, day), nil)
	if err == nil && code != 0 {
		err = fmt.Errorf("%s exited %d: %s", backupCmd(ext, day), code, strings.TrimSpace(stderr+stdout))
	}
	if err == nil {
		err = mt.CheckOutput(stdout + stderr)
	}
	if err != nil {
		return 0, err
	}
	remote, err := waitForFile(ctx, conn, mt.BackupBase(day)+"."+ext)
	if err != nil {
		return 0, err
	}

	// Stream SFTP straight into the sink; neither side holds the whole file.
	pr, pw := io.Pipe()
	go func() {
		_, err := conn.Download(ctx, remote, pw)
		pw.CloseWithError(err)
	}()
	n, err := sink.Write(ctx, key, pr)
	_ = pr.CloseWithError(err) // unblock the download if the sink failed
	return n, err
}

// waitForFile polls until the file exists (at the root or under flash/) with
// a non-zero size that holds steady across two polls.
func waitForFile(ctx context.Context, conn *ssh.Conn, file string) (string, error) {
	deadline := time.Now().Add(fileWaitTimeout)
	last := map[string]int64{}
	for {
		for _, p := range mt.RemoteCandidates(file) {
			size, err := conn.Stat(p)
			if err != nil || size == 0 {
				continue
			}
			if prev, ok := last[p]; ok && prev == size {
				return p, nil
			}
			last[p] = size
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not appear on the device within %s", file, fileWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(fileWaitInterval):
		}
	}
}

func confirmTargets(cmd *cobra.Command, action string, targets []runner.Target) error {
	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "About to %s %d device(s):\n", action, len(targets))
	for _, t := range targets {
		_, _ = fmt.Fprintf(w, "  %s (%s)\n", t.Name, net.JoinHostPort(t.Host.Host, strconv.Itoa(t.Port)))
	}
	if !askYesNo(cmd, "Continue? [y/N]: ") {
		return errors.New("aborted")
	}
	return nil
}

func newMtRebootCmd(opts *globalOptions) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "reboot",
		Short: "Reboot devices (asks for confirmation unless --confirm)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := mtTargets(cmd, opts)
			if err != nil {
				return err
			}
			if opts.dryRun {
				return printDryRun(cmd.OutOrStdout(), targets, mt.RebootCmd, nil)
			}
			if !confirm {
				if err := confirmTargets(cmd, "reboot", targets); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			if err := prepareConnect(ctx, opts, targets); err != nil {
				return err
			}
			results := runner.Execute(ctx, targets, opts.parallel, runner.WithConn(connOptions(opts),
				func(ctx context.Context, conn *ssh.Conn, _ runner.Target, res *runner.Result) {
					if err := expectDisconnect(conn.Run(ctx, mt.RebootCmd, nil)); err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					res.Stdout = "rebooting"
				}))
			if err := runner.Write(cmd.OutOrStdout(), opts.output, results); err != nil {
				return err
			}
			return outcome(ctx, results)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "skip the confirmation prompt")
	return cmd
}

// expectDisconnect accepts a command that ends the session (reboot,
// install) as success, as well as a clean exit 0.
func expectDisconnect(stdout, stderr string, code int, err error) error {
	switch {
	case errors.Is(err, ssh.ErrDisconnected):
		return nil
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("exited %d: %s", code, strings.TrimSpace(stderr+stdout))
	}
	return mt.CheckOutput(stdout + stderr)
}

func newMtUpgradeCmd(opts *globalOptions) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Check and install RouterOS updates, then routerboard firmware",
		Long: "Each run takes one step per device, since each step ends in a reboot:\n" +
			"  1. a newer RouterOS on the channel: install it (the device reboots)\n" +
			"  2. RouterOS current but routerboard firmware behind: upgrade firmware and reboot\n" +
			"  3. otherwise: report up to date\n" +
			"Run it again after devices come back to finish step 2.\n" +
			"With --dry-run it only checks for updates and reports what it would do.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := mtTargets(cmd, opts)
			if err != nil {
				return err
			}
			dry := opts.dryRun
			if !dry && !confirm {
				if err := confirmTargets(cmd, "upgrade (and reboot)", targets); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			if err := prepareConnect(ctx, opts, targets); err != nil {
				return err
			}
			results := runner.Execute(ctx, targets, opts.parallel, runner.WithConn(connOptions(opts),
				func(ctx context.Context, conn *ssh.Conn, _ runner.Target, res *runner.Result) {
					msg, err := upgradeOne(ctx, conn, dry)
					if err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					res.Stdout = msg
				}))
			if err := runner.Write(cmd.OutOrStdout(), opts.output, results); err != nil {
				return err
			}
			return outcome(ctx, results)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "skip the confirmation prompt")
	return cmd
}

// upgradeOne performs (or with dry, describes) the next upgrade step.
func upgradeOne(ctx context.Context, conn *ssh.Conn, dry bool) (string, error) {
	if _, _, _, err := conn.Run(ctx, mt.CheckUpdatesCmd, nil); err != nil {
		return "", err
	}
	status, err := pollUpdate(ctx, conn)
	if err != nil {
		return "", err
	}
	if status.Available() {
		if dry {
			return fmt.Sprintf("would install RouterOS %s -> %s", status.Installed, status.Latest), nil
		}
		if err := expectDisconnect(conn.Run(ctx, mt.InstallUpdatesCmd, nil)); err != nil {
			return "", err
		}
		return fmt.Sprintf("installing RouterOS %s -> %s (device reboots; run again for firmware)", status.Installed, status.Latest), nil
	}

	rbOut, _, _, err := conn.Run(ctx, mt.RouterboardCmd, nil)
	if err != nil {
		return "", err
	}
	v, _ := mt.ParseVersion(rbOut, "ros="+status.Installed)
	if !v.FirmwarePending() {
		return fmt.Sprintf("up to date (RouterOS %s)", status.Installed), nil
	}
	if dry {
		return fmt.Sprintf("would upgrade firmware %s -> %s and reboot", v.FirmwareRB, v.UpgradeFW), nil
	}
	out, errOut, code, err := conn.Run(ctx, mt.FirmwareUpgrade, nil)
	if err == nil && code != 0 {
		err = fmt.Errorf("firmware upgrade exited %d: %s", code, strings.TrimSpace(errOut+out))
	}
	if err == nil {
		err = mt.CheckOutput(out + errOut)
	}
	if err != nil {
		return "", err
	}
	if err := expectDisconnect(conn.Run(ctx, mt.RebootCmd, nil)); err != nil {
		return "", err
	}
	return fmt.Sprintf("firmware %s -> %s, rebooting", v.FirmwareRB, v.UpgradeFW), nil
}

// pollUpdate waits for check-for-updates to finish.
func pollUpdate(ctx context.Context, conn *ssh.Conn) (mt.UpdateStatus, error) {
	deadline := time.Now().Add(updateWait)
	for {
		out, _, _, err := conn.Run(ctx, mt.UpdateStatusCmd, nil)
		if err != nil {
			return mt.UpdateStatus{}, err
		}
		status, err := mt.ParseUpdate(out)
		if err != nil {
			return mt.UpdateStatus{}, err
		}
		if !status.Checking() {
			return status, nil
		}
		if time.Now().After(deadline) {
			return mt.UpdateStatus{}, fmt.Errorf("update check still running after %s", updateWait)
		}
		select {
		case <-ctx.Done():
			return mt.UpdateStatus{}, ctx.Err()
		case <-time.After(updatePoll):
		}
	}
}

func newMtVersionCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show routerboard firmware and installed package versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := mtTargets(cmd, opts)
			if err != nil {
				return err
			}
			if opts.dryRun {
				return printDryRun(cmd.OutOrStdout(), targets, mt.RouterboardCmd+"\n"+mt.VersionScript, nil)
			}
			ctx := cmd.Context()
			if err := prepareConnect(ctx, opts, targets); err != nil {
				return err
			}
			results := runner.Execute(ctx, targets, opts.parallel, runner.WithConn(connOptions(opts),
				func(ctx context.Context, conn *ssh.Conn, _ runner.Target, res *runner.Result) {
					// CHR/x86 has no routerboard; its print error is not fatal.
					rb, _, _, err := conn.Run(ctx, mt.RouterboardCmd, nil)
					if err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					script, _, _, err := conn.Run(ctx, mt.VersionScript, nil)
					if err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					v, err := mt.ParseVersion(rb, script)
					if err != nil {
						res.Err, res.ExitCode = err, -1
						return
					}
					res.Data = v
				}))
			if err := writeVersions(cmd.OutOrStdout(), opts.output, results); err != nil {
				return err
			}
			return outcome(ctx, results)
		},
	}
}

// versionRecord is the json/csv shape of `mt version`.
type versionRecord struct {
	Host  string `json:"host"`
	Name  string `json:"name"`
	Group string `json:"group"`
	mt.Version
	Error string `json:"error,omitempty"`
}

func writeVersions(w io.Writer, format string, results []runner.Result) error {
	recs := make([]versionRecord, len(results))
	for i, r := range results {
		recs[i] = versionRecord{Host: r.Host, Name: r.Name, Group: r.Group}
		if v, ok := r.Data.(mt.Version); ok {
			recs[i].Version = v
		}
		if r.Err != nil {
			recs[i].Error = r.Err.Error()
		}
		if recs[i].Packages == nil {
			recs[i].Packages = []string{}
		}
	}
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(recs)
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"host", "name", "group", "board", "rb_firmware", "upgrade_firmware", "ros_version", "packages", "error"})
		for _, r := range recs {
			_ = cw.Write([]string{r.Host, r.Name, r.Group, r.Board, r.FirmwareRB, r.UpgradeFW, r.ROS, strings.Join(r.Packages, " "), r.Error})
		}
		cw.Flush()
		return cw.Error()
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "HOST\tNAME\tRB FIRMWARE\tUPGRADE FW\tROS VERSION\tPACKAGES")
	for _, r := range recs {
		if r.Error != "" {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t[error] %s\n", r.Host, r.Name, strings.ReplaceAll(r.Error, "\n", " "))
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Host, r.Name, dash(r.FirmwareRB), dash(r.UpgradeFW), r.ROS, strings.Join(r.Packages, ", "))
	}
	return tw.Flush()
}
