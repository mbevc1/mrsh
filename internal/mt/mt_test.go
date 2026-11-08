package mt

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// Captured from a hAP ac² on RouterOS 7.13.2.
const routerboardSample = `       routerboard: yes
        board-name: hAP ac^2
             model: RBD52G-5HacD2HnD
          revision: r2
     serial-number: D0000000000A
     firmware-type: ipq4000L
  factory-firmware: 6.45.9
  current-firmware: 7.13.2
  upgrade-firmware: 7.14
`

const versionScriptSample = "ros=7.14 (stable)\r\npkg=routeros 7.14\r\npkg=wifi-qcom-ac 7.14\r\n"

const updateSample = `            channel: stable
  installed-version: 7.13.2
     latest-version: 7.14
             status: New version is available
`

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion(routerboardSample, versionScriptSample)
	if err != nil {
		t.Fatal(err)
	}
	want := Version{Board: "RBD52G-5HacD2HnD", FirmwareRB: "7.13.2", UpgradeFW: "7.14", ROS: "7.14",
		Packages: []string{"routeros", "wifi-qcom-ac"}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("got %+v\nwant %+v", v, want)
	}
	if !v.FirmwarePending() {
		t.Error("firmware 7.13.2 < 7.14 should be pending")
	}
}

func TestParseVersionCHR(t *testing.T) {
	// CHR/x86 has no routerboard; its print fails and returns an error line.
	v, err := ParseVersion("", "ros=7.14.3 (stable)\npkg=routeros 7.14.3\n")
	if err != nil || v.ROS != "7.14.3" || v.Board != "" || v.FirmwarePending() {
		t.Errorf("v=%+v err=%v", v, err)
	}
}

func TestParseVersionError(t *testing.T) {
	_, err := ParseVersion("", "bad command name resource (line 1 column 18)\n")
	if !errors.Is(err, ErrUnexpectedOutput) {
		t.Errorf("err = %v", err)
	}
}

func TestParseUpdate(t *testing.T) {
	u, err := ParseUpdate(updateSample)
	if err != nil {
		t.Fatal(err)
	}
	if u != (UpdateStatus{Channel: "stable", Installed: "7.13.2", Latest: "7.14", Status: "New version is available"}) {
		t.Errorf("u = %+v", u)
	}
	if !u.Available() || u.Checking() {
		t.Errorf("available=%v checking=%v", u.Available(), u.Checking())
	}
	current, _ := ParseUpdate("installed-version: 7.14\nlatest-version: 7.14\nstatus: System is already up to date\n")
	if current.Available() {
		t.Error("up-to-date reported available")
	}
	checking, _ := ParseUpdate("installed-version: 7.14\nstatus: finding out latest version...\n")
	if !checking.Checking() {
		t.Error("in-progress check not detected")
	}
	if _, err := ParseUpdate("expected end of command\n"); !errors.Is(err, ErrUnexpectedOutput) {
		t.Errorf("err = %v", err)
	}
}

func TestCommands(t *testing.T) {
	mon := time.Date(2026, 4, 6, 3, 0, 0, 0, time.UTC) // a Monday
	if d := Weekday(mon); d != "Mon" {
		t.Fatalf("Weekday = %s", d)
	}
	if got := ExportCmd("Mon"); got != "/export show-sensitive file=backup-Mon" {
		t.Errorf("ExportCmd = %q", got)
	}
	if got := SaveBackupCmd("Sun"); got != "/system backup save name=backup-Sun" {
		t.Errorf("SaveBackupCmd = %q", got)
	}
}

func TestFormats(t *testing.T) {
	for in, want := range map[string][]string{"rsc": {"rsc"}, "backup": {"backup"}, "both": {"rsc", "backup"}, "": {"rsc", "backup"}} {
		if got, err := Formats(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Formats(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := Formats("zip"); err == nil {
		t.Error("bad format accepted")
	}
}

func TestBackupKey(t *testing.T) {
	d := time.Date(2026, 4, 3, 23, 59, 0, 0, time.UTC)
	tests := []struct {
		group, name string
		literal     bool
		want        string
	}{
		{"routers", "rb01", false, "routers/rb01/2026-04-03.rsc"},
		{"", "rb01", false, "_ungrouped/rb01/2026-04-03.rsc"},
		{"ignored", "admin@10.0.0.5:2222", true, "_literal/admin@10.0.0.5_2222/2026-04-03.rsc"},
		{"../etc", "..", false, "_etc/_/2026-04-03.rsc"},
		{"a/b", "c", false, "a_b/c/2026-04-03.rsc"},
	}
	for _, tt := range tests {
		if got := BackupKey(tt.group, tt.name, tt.literal, d, "rsc"); got != tt.want {
			t.Errorf("BackupKey(%q,%q,%v) = %q, want %q", tt.group, tt.name, tt.literal, got, tt.want)
		}
	}
}

func TestParseKV(t *testing.T) {
	kv := ParseKV("  a: 1\r\n junk line\nbad key: x\n b:  two words \n")
	if kv["a"] != "1" || kv["b"] != "two words" || len(kv) != 2 {
		t.Errorf("kv = %v", kv)
	}
}
