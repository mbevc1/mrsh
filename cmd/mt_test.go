package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/sshtest"
)

const rbSample = `  current-firmware: 7.13.2
  upgrade-firmware: 7.14
             model: RB5009UG+S+
`

// fakeRouter answers RouterOS commands and writes backup files into the
// SFTP root the way a device would.
type fakeRouter struct {
	root string
	mu   sync.Mutex
	cmds []string

	update    []string // successive UpdateStatusCmd replies
	rb        string
	exportErr string
}

func (f *fakeRouter) exec(cmd string) sshtest.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "/export show-sensitive file="):
		if f.exportErr != "" {
			return sshtest.Reply{Stdout: f.exportErr}
		}
		name := strings.TrimPrefix(cmd, "/export show-sensitive file=") + ".rsc"
		_ = os.WriteFile(filepath.Join(f.root, name), []byte("# export\n/ip address add address=10.0.0.1/24\n"), 0o600)
	case strings.HasPrefix(cmd, "/system backup save name="):
		// Some boards store files under flash/.
		name := strings.TrimPrefix(cmd, "/system backup save name=") + ".backup"
		_ = os.MkdirAll(filepath.Join(f.root, "flash", mt.BackupDir), 0o700)
		_ = os.WriteFile(filepath.Join(f.root, "flash", name), []byte("BINARYBACKUP"), 0o600)
		return sshtest.Reply{Stdout: "Configuration backup saved\n"}
	case cmd == mt.EnsureBackupDirCmd:
		_ = os.MkdirAll(filepath.Join(f.root, mt.BackupDir), 0o700)
	case cmd == mt.RebootCmd, cmd == mt.InstallUpdatesCmd:
		return sshtest.Reply{Drop: true}
	case cmd == mt.UpdateStatusCmd:
		out := f.update[0]
		if len(f.update) > 1 {
			f.update = f.update[1:]
		}
		return sshtest.Reply{Stdout: out}
	case cmd == mt.RouterboardCmd:
		return sshtest.Reply{Stdout: f.rb}
	case cmd == mt.VersionScript:
		return sshtest.Reply{Stdout: "ros=7.14 (stable)\npkg=routeros 7.14\npkg=container 7.14\n"}
	case cmd == mt.CheckUpdatesCmd, cmd == mt.FirmwareUpgrade:
	default:
		return sshtest.Reply{Stdout: "bad command name\n"}
	}
	return sshtest.Reply{}
}

func (f *fakeRouter) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeRouter) sentContains(cmd string) bool {
	for _, c := range f.sent() {
		if c == cmd {
			return true
		}
	}
	return false
}

// startRouter runs a fake device and returns it with a config naming it rt1
// in group lab.
func startRouter(t *testing.T) (*fakeRouter, string) {
	t.Helper()
	f := &fakeRouter{root: t.TempDir(), rb: rbSample,
		update: []string{"installed-version: 7.14\nlatest-version: 7.14\nstatus: System is already up to date\n"}}
	srv := sshtest.Start(t, sshtest.Options{User: "admin", Password: "pw", Exec: f.exec, SFTPRoot: f.root})
	cfg := writeConfig(t, fmt.Sprintf("defaults: {user: admin}\nhosts:\n  - {name: rt1, host: %s, port: %d, group: lab, pass: pw}\n", srv.Host, srv.Port))

	monday := time.Date(2026, 4, 6, 3, 0, 0, 0, time.Local)
	oldNow, oldInterval, oldPoll := now, fileWaitInterval, updatePoll
	now, fileWaitInterval, updatePoll = func() time.Time { return monday }, 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { now, fileWaitInterval, updatePoll = oldNow, oldInterval, oldPoll })
	return f, cfg
}

func TestMtBackupBothFormats(t *testing.T) {
	f, cfg := startRouter(t)
	dest := t.TempDir()
	out, err := execRoot(t, "-f", cfg, "mt", "backup", "--path", dest)
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	rsc, err := os.ReadFile(filepath.Join(dest, "lab", "rt1", "Mon.rsc"))
	if err != nil || !strings.Contains(string(rsc), "/ip address add") {
		t.Errorf("rsc = %q, %v", rsc, err)
	}
	bin, err := os.ReadFile(filepath.Join(dest, "lab", "rt1", "Mon.backup"))
	if err != nil || string(bin) != "BINARYBACKUP" {
		t.Errorf("backup = %q, %v (flash/ fallback)", bin, err)
	}
	// The weekday files stay on the device as the rolling set.
	for _, p := range []string{"backups/Mon.rsc", "flash/backups/Mon.backup"} {
		if _, err := os.Stat(filepath.Join(f.root, p)); err != nil {
			t.Errorf("device file %s removed: %v", p, err)
		}
	}
	if !strings.Contains(out, "saved lab/rt1/Mon.rsc (45 B), Mon.backup (12 B)") || strings.Contains(out, "lines)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestMtBackupRSCOnlyAndJSON(t *testing.T) {
	f, cfg := startRouter(t)
	dest := t.TempDir()
	out, err := execRoot(t, "-f", cfg, "-o", "json", "mt", "backup", "--format", "rsc", "--path", dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.sent() {
		if strings.HasPrefix(c, "/system backup save") {
			t.Errorf("binary backup ran with --format rsc")
		}
	}
	var recs []map[string]any
	if err := json.Unmarshal([]byte(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "lab", "rt1", "Mon.backup")); err == nil {
		t.Error(".backup written with --format rsc")
	}
}

func TestMtBackupDeviceError(t *testing.T) {
	f, cfg := startRouter(t)
	f.exportErr = "failure: not enough disk space\n"
	_, err := execRoot(t, "-f", cfg, "mt", "backup", "--path", t.TempDir())
	if exitCode(err) != exitHostFailed {
		t.Errorf("exit = %d (err %v)", exitCode(err), err)
	}
}

func TestMtBackupUsageErrors(t *testing.T) {
	f, cfg := startRouter(t)
	for _, args := range [][]string{
		{"mt", "backup", "--path", "s3:///prefix/"},
		{"mt", "backup", "--format", "zip"},
		{"mt", "backup", "--sse", "rot13"},
	} {
		_, err := execRoot(t, append([]string{"-f", cfg}, args...)...)
		if err == nil || exitCode(err) != exitUsage {
			t.Errorf("%v: err = %v", args, err)
		}
	}
	if len(f.sent()) != 0 {
		t.Errorf("commands ran despite usage errors: %v", f.sent())
	}
}

func TestMtBackupDryRun(t *testing.T) {
	f, cfg := startRouter(t)
	out, err := execRoot(t, "-f", cfg, "--dry-run", "mt", "backup")
	if err != nil || !strings.Contains(out, "/export show-sensitive file=backups/Mon") || !strings.Contains(out, "/system backup save name=backups/Mon") || !strings.Contains(out, "/file add type=directory") {
		t.Errorf("err=%v\n%s", err, out)
	}
	if len(f.sent()) != 0 {
		t.Errorf("dry-run connected: %v", f.sent())
	}
}

func TestMtReboot(t *testing.T) {
	f, cfg := startRouter(t)

	// Declined prompt: nothing runs.
	root := newRootCmd()
	root.SetIn(strings.NewReader("n\n"))
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"-f", cfg, "mt", "reboot"})
	if err := root.Execute(); err == nil || err.Error() != "aborted" {
		t.Errorf("declined: err = %v", err)
	}
	// Dry-run: nothing runs.
	if out, err := execRoot(t, "-f", cfg, "--dry-run", "mt", "reboot"); err != nil || !strings.Contains(out, mt.RebootCmd) {
		t.Errorf("dry-run: err=%v\n%s", err, out)
	}
	if len(f.sent()) != 0 {
		t.Fatalf("commands ran: %v", f.sent())
	}

	// The dropped session counts as success.
	out, err := execRoot(t, "-f", cfg, "mt", "reboot", "-y")
	if err != nil || !strings.Contains(out, "rebooting") || !f.sentContains(mt.RebootCmd) {
		t.Errorf("reboot: err=%v\n%s", err, out)
	}
}

func TestMtUpgradeInstallsPackages(t *testing.T) {
	f, cfg := startRouter(t)
	f.update = []string{
		"installed-version: 7.13.2\nstatus: finding out latest version...\n",
		"installed-version: 7.13.2\nlatest-version: 7.14\nstatus: New version is available\n",
	}
	out, err := execRoot(t, "-f", cfg, "mt", "upgrade", "-y")
	if err != nil || !strings.Contains(out, "installing RouterOS 7.13.2 -> 7.14") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if !f.sentContains(mt.InstallUpdatesCmd) || f.sentContains(mt.FirmwareUpgrade) {
		t.Errorf("commands = %v", f.sent())
	}
}

func TestMtUpgradeDryRunOnlyChecks(t *testing.T) {
	f, cfg := startRouter(t)
	f.update = []string{"installed-version: 7.13.2\nlatest-version: 7.14\nstatus: New version is available\n"}
	out, err := execRoot(t, "-f", cfg, "--dry-run", "mt", "upgrade")
	if err != nil || !strings.Contains(out, "would install RouterOS 7.13.2 -> 7.14") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if f.sentContains(mt.InstallUpdatesCmd) || f.sentContains(mt.RebootCmd) {
		t.Errorf("dry-run changed the device: %v", f.sent())
	}
}

func TestMtUpgradeFirmwareStep(t *testing.T) {
	f, cfg := startRouter(t) // RouterOS current, firmware 7.13.2 -> 7.14 pending
	out, err := execRoot(t, "-f", cfg, "mt", "upgrade", "-y")
	if err != nil || !strings.Contains(out, "firmware 7.13.2 -> 7.14, rebooting") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if !f.sentContains(mt.FirmwareUpgrade) || !f.sentContains(mt.RebootCmd) || f.sentContains(mt.InstallUpdatesCmd) {
		t.Errorf("commands = %v", f.sent())
	}

	f.rb = "current-firmware: 7.14\nupgrade-firmware: 7.14\n"
	if out, err := execRoot(t, "-f", cfg, "mt", "upgrade", "-y"); err != nil || !strings.Contains(out, "up to date (RouterOS 7.14)") {
		t.Errorf("up to date: err=%v\n%s", err, out)
	}
}

func TestMtVersion(t *testing.T) {
	_, cfg := startRouter(t)
	out, err := execRoot(t, "-f", cfg, "mt", "version")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOST", "RB FIRMWARE", "rt1", "7.13.2", "7.14", "container, routeros"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	out, err = execRoot(t, "-f", cfg, "-o", "json", "mt", "version")
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if recs[0]["ros_version"] != "7.14" || recs[0]["rb_firmware"] != "7.13.2" || recs[0]["name"] != "rt1" {
		t.Errorf("record = %v", recs[0])
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 14336: "14.0 KiB",
		1572864: "1.5 MiB", 3 << 30: "3.0 GiB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestAssumeYesSpellings(t *testing.T) {
	for _, flag := range []string{"-y", "--assume-yes", "--yes"} {
		f, cfg := startRouter(t)
		if out, err := execRoot(t, "-f", cfg, "mt", "reboot", flag); err != nil || !f.sentContains(mt.RebootCmd) {
			t.Errorf("%s: err=%v\n%s", flag, err, out)
		}
	}
}
