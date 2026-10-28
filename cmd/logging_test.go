package cmd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestPlainWarnings(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var buf bytes.Buffer
	setupLogging(&buf, false, false)
	slog.Info("hidden")
	slog.With("host", "web01").Warn("slow", "ms", 1200)
	slog.Error("broken")
	want := "mrsh: warning: slow host=web01 ms=1200\nmrsh: error: broken\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestFullOutputAccepted(t *testing.T) {
	out, err := execRoot(t, "-o", "full", "version")
	if err != nil || out == "" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

// runSplit runs mrsh and returns stdout and stderr separately.
func runSplit(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestWarningsStayOnStderr(t *testing.T) {
	cfg := writeConfig(t, "credentials:\n  lab: {user: a, pass: setpw}\nhosts:\n  - {name: a, host: 10.0.0.1, pass: hostpw}\n  - {name: b, host: 10.0.0.2, credential: lab}\n")

	stdout, stderr, err := runSplit(t, "-f", cfg, "-o", "json", "hosts", "list")
	var rows []map[string]any
	if err != nil || json.Unmarshal([]byte(stdout), &rows) != nil || len(rows) != 2 {
		t.Fatalf("stdout must be clean JSON: err=%v\n%s", err, stdout)
	}
	if !strings.Contains(stderr, "mrsh: warning: ") || !strings.Contains(stderr, "2 literal password(s) (credential lab, host a)") {
		t.Errorf("stderr = %q", stderr)
	}

	// -q drops warnings, even with MRSH_DEBUG set; output is unchanged.
	t.Setenv("MRSH_DEBUG", "1")
	quietOut, quietErr, err := runSplit(t, "-q", "-f", cfg, "-o", "json", "hosts", "list")
	if err != nil || quietErr != "" || quietOut != stdout {
		t.Errorf("-q: err=%v stderr=%q", err, quietErr)
	}

	// Errors still reach stderr (and only stderr) under -q.
	stdout, stderr, err = runSplit(t, "-q", "-f", cfg+".missing", "hosts", "list")
	if err == nil || stdout != "" || !strings.Contains(stderr, "Error: config") {
		t.Errorf("error: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}

	if _, _, err := runSplit(t, "-q", "-d", "version"); err == nil {
		t.Error("-q with -d accepted")
	}
}

func TestPassFlagWarns(t *testing.T) {
	cfg := writeConfig(t, "hosts: []\n")
	_, stderr, err := runSplit(t, "-f", cfg, "hosts", "add", "--name", "a", "-H", "10.0.0.1", "--pass", "x")
	if err != nil || !strings.Contains(stderr, "--pass stores the password in the config in the clear") {
		t.Errorf("err=%v stderr=%q", err, stderr)
	}
	if _, stderr, _ = runSplit(t, "-q", "-f", cfg, "hosts", "add", "--name", "b", "-H", "10.0.0.2", "--pass", "x"); stderr != "" {
		t.Errorf("-q stderr = %q", stderr)
	}
}
