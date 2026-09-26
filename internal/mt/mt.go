// Package mt holds MikroTik RouterOS command strings and output parsing.
// It targets RouterOS v7.
package mt

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Backup formats selected by --format.
const (
	FormatRSC    = "rsc"    // text export
	FormatBackup = "backup" // binary system backup
	FormatBoth   = "both"
)

// RebootCmd reboots the device; the session drops before an exit status.
const RebootCmd = "/system reboot"

// Package and firmware update commands.
const (
	CheckUpdatesCmd   = "/system package update check-for-updates"
	UpdateStatusCmd   = "/system package update print"
	InstallUpdatesCmd = "/system package update install" // reboots when done
	RouterboardCmd    = "/system routerboard print"
	FirmwareUpgrade   = "/system routerboard upgrade" // applies on next reboot
)

// VersionScript prints the RouterOS version and each enabled package as
// "key=value" lines. Scripted :put output is stable across v7 releases,
// unlike the column layout of "/system package print".
const VersionScript = `:put ("ros=" . [/system resource get version]); ` +
	`:foreach p in=[/system package find where !disabled] do={` +
	`:put ("pkg=" . [/system package get $p name] . " " . [/system package get $p version])}`

// Weekday returns the device-side rolling backup day ("Mon" … "Sun").
func Weekday(t time.Time) string { return t.Weekday().String()[:3] }

// BackupBase is the on-device file name (without extension) for a weekday.
func BackupBase(dow string) string { return "backup-" + dow }

// ExportCmd writes the text export. v7 exports compact by default; the
// "compact" keyword is v6 syntax.
func ExportCmd(dow string) string { return "/export show-sensitive file=" + BackupBase(dow) }

// SaveBackupCmd writes the binary backup.
func SaveBackupCmd(dow string) string { return "/system backup save name=" + BackupBase(dow) }

// Formats expands a --format value to file extensions, in download order.
func Formats(format string) ([]string, error) {
	switch format {
	case FormatRSC:
		return []string{"rsc"}, nil
	case FormatBackup:
		return []string{"backup"}, nil
	case FormatBoth, "":
		return []string{"rsc", "backup"}, nil
	}
	return nil, fmt.Errorf("invalid --format %q: must be rsc, backup or both", format)
}

// RemoteCandidates lists where RouterOS may have written a file: the root,
// or flash/ on boards whose storage is mounted there.
func RemoteCandidates(file string) []string { return []string{file, "flash/" + file} }

// BackupKey is the sink key <group>/<name>/<YYYY-MM-DD>.<ext>. Hosts with no
// group use "_ungrouped"; literal --host targets use "_literal/<address>".
func BackupKey(group, name string, literal bool, date time.Time, ext string) string {
	switch {
	case literal:
		group = "_literal"
	case group == "":
		group = "_ungrouped"
	}
	return path.Join(safeSegment(group), safeSegment(name), date.Format("2006-01-02")+"."+ext)
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._@-]+`)

// safeSegment makes s usable as one path segment: no separators, and no
// leading dots (which rules out "." and ".." and hidden names).
func safeSegment(s string) string {
	s = strings.TrimLeft(unsafeChars.ReplaceAllString(s, "_"), ".")
	if s == "" {
		return "_"
	}
	return s
}

// ParseKV parses RouterOS "key: value" print output (as from
// "/system routerboard print"). Lines without a colon are ignored.
func ParseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		m[k] = strings.TrimSpace(strings.TrimRight(v, "\r"))
	}
	return m
}

// Version is one device's firmware and package state.
type Version struct {
	Board      string   `json:"board,omitempty"`
	FirmwareRB string   `json:"rb_firmware,omitempty"`
	UpgradeFW  string   `json:"upgrade_firmware,omitempty"`
	ROS        string   `json:"ros_version"`
	Packages   []string `json:"packages"`
}

// ErrUnexpectedOutput means the device replied with something we could not
// parse (often a RouterOS error such as "bad command name").
var ErrUnexpectedOutput = errors.New("unexpected RouterOS output")

// ParseVersion combines "/system routerboard print" output (empty on CHR or
// x86, which have no routerboard) with the VersionScript output.
func ParseVersion(routerboard, script string) (Version, error) {
	var v Version
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ros="):
			v.ROS = strings.TrimSuffix(strings.TrimPrefix(line, "ros="), " (stable)")
		case strings.HasPrefix(line, "pkg="):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "pkg="), " ")
			v.Packages = append(v.Packages, name)
		}
	}
	if v.ROS == "" {
		return Version{}, fmt.Errorf("%w: %q", ErrUnexpectedOutput, firstLine(script))
	}
	sort.Strings(v.Packages)
	rb := ParseKV(routerboard)
	v.Board = rb["model"]
	if v.Board == "" {
		v.Board = rb["board-name"]
	}
	v.FirmwareRB, v.UpgradeFW = rb["current-firmware"], rb["upgrade-firmware"]
	return v, nil
}

// FirmwarePending reports whether the routerboard firmware lags the
// installed RouterOS (applied by "routerboard upgrade" plus a reboot).
func (v Version) FirmwarePending() bool {
	return v.FirmwareRB != "" && v.UpgradeFW != "" && v.FirmwareRB != v.UpgradeFW
}

// UpdateStatus is the parsed "/system package update print" output.
type UpdateStatus struct {
	Channel   string `json:"channel"`
	Installed string `json:"installed_version"`
	Latest    string `json:"latest_version"`
	Status    string `json:"status"`
}

// ParseUpdate parses UpdateStatusCmd output.
func ParseUpdate(out string) (UpdateStatus, error) {
	kv := ParseKV(out)
	u := UpdateStatus{Channel: kv["channel"], Installed: kv["installed-version"], Latest: kv["latest-version"], Status: kv["status"]}
	if u.Installed == "" {
		return UpdateStatus{}, fmt.Errorf("%w: %q", ErrUnexpectedOutput, firstLine(out))
	}
	return u, nil
}

// Checking reports whether check-for-updates is still running.
func (u UpdateStatus) Checking() bool {
	return strings.Contains(strings.ToLower(u.Status), "finding out")
}

// Available reports whether a newer RouterOS is on the channel.
func (u UpdateStatus) Available() bool { return u.Latest != "" && u.Latest != u.Installed }

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// routerOSErrors are phrases RouterOS prints (often with exit status 0)
// when a command fails.
var routerOSErrors = []string{"failure:", "bad command name", "syntax error", "expected end of command", "no such item", "input does not match"}

// CheckOutput returns an error when out carries a RouterOS error message.
func CheckOutput(out string) error {
	lower := strings.ToLower(out)
	for _, p := range routerOSErrors {
		if strings.Contains(lower, p) {
			return fmt.Errorf("routeros: %s", firstLine(out))
		}
	}
	return nil
}
