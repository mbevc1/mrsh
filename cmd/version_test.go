package cmd

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	tests := []struct {
		name                        string
		ldVersion, ldCommit, ldDate string
		bi                          *debug.BuildInfo
		wantVer, wantRev, wantWhen  string
	}{
		{
			name:      "no build info",
			ldVersion: "dev",
			wantVer:   "dev",
		},
		{
			name:      "ldflags win over build info",
			ldVersion: "v0.1.0", ldCommit: "abc1234", ldDate: "2026-04-03T10:00:00Z",
			bi:      buildInfo("v9.9.9", "fffffff", "2020-01-01T00:00:00Z", "false"),
			wantVer: "v0.1.0", wantRev: "abc1234", wantWhen: "2026-04-03T10:00:00Z",
		},
		{
			name:      "ldflags version is not re-marked dirty",
			ldVersion: "v0.1.0-dirty",
			bi:        buildInfo("(devel)", "fffffff", "2020-01-01T00:00:00Z", "true"),
			wantVer:   "v0.1.0-dirty", wantRev: "fffffff", wantWhen: "2020-01-01T00:00:00Z",
		},
		{
			name:      "module version from go install",
			ldVersion: "dev",
			bi:        buildInfo("v1.2.3", "", "", ""),
			wantVer:   "v1.2.3",
		},
		{
			name:      "devel build falls back to dev with vcs stamps",
			ldVersion: "dev",
			bi:        buildInfo("(devel)", "deadbeef", "2026-01-02T03:04:05Z", "false"),
			wantVer:   "dev", wantRev: "deadbeef", wantWhen: "2026-01-02T03:04:05Z",
		},
		{
			name:      "go 1.24 pseudo-version already marked dirty",
			ldVersion: "dev",
			bi:        buildInfo("v0.0.0-20260412200954-7ca440b22539+dirty", "7ca440b", "", "true"),
			wantVer:   "v0.0.0-20260412200954-7ca440b22539+dirty", wantRev: "7ca440b",
		},
		{
			name:      "dirty tree marks derived version",
			ldVersion: "dev",
			bi:        buildInfo("(devel)", "deadbeef", "", "true"),
			wantVer:   "dev-dirty", wantRev: "deadbeef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withBuildVars(t, tt.ldVersion, tt.ldCommit, tt.ldDate, tt.bi)
			ver, rev, when := resolveVersion()
			if ver != tt.wantVer || rev != tt.wantRev || when != tt.wantWhen {
				t.Errorf("resolveVersion() = (%q, %q, %q), want (%q, %q, %q)",
					ver, rev, when, tt.wantVer, tt.wantRev, tt.wantWhen)
			}
		})
	}
}

func TestVersionJSON(t *testing.T) {
	withBuildVars(t, "v0.1.0", "abc1234", "2026-04-03T10:00:00Z", nil)
	builtBy = "test"

	out, err := execRoot(t, "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	for _, k := range []string{"name", "version", "commit", "date", "builtBy", "go", "os", "arch"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %v", k, got)
		}
	}
	if got["name"] != "mrsh" || got["version"] != "v0.1.0" || got["commit"] != "abc1234" || got["builtBy"] != "test" {
		t.Errorf("unexpected values: %v", got)
	}
}

func TestVersionText(t *testing.T) {
	withBuildVars(t, "v0.1.0", "abc1234", "", nil)
	out, err := execRoot(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(out), []byte("mrsh v0.1.0\n  commit:  abc1234\n  built:   unknown\n")) {
		t.Errorf("unexpected text output:\n%s", out)
	}
}

func TestVersionRejectsCSV(t *testing.T) {
	if _, err := execRoot(t, "version", "-o", "csv"); err == nil {
		t.Fatal("expected error for csv output")
	}
}

func buildInfo(mainVersion, rev, when, modified string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/mbevc1/mrsh", Version: mainVersion}}
	for k, v := range map[string]string{"vcs.revision": rev, "vcs.time": when, "vcs.modified": modified} {
		if v != "" {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
		}
	}
	return bi
}

// withBuildVars swaps the ldflags vars and build info for one test.
func withBuildVars(t *testing.T, ver, rev, when string, bi *debug.BuildInfo) {
	t.Helper()
	oldVer, oldRev, oldWhen, oldBy, oldRead := version, commit, date, builtBy, readBuildInfo
	t.Cleanup(func() {
		version, commit, date, builtBy, readBuildInfo = oldVer, oldRev, oldWhen, oldBy, oldRead
	})
	version, commit, date = ver, rev, when
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, bi != nil }
}
