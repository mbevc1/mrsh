package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/sshtest"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverConfig(t *testing.T) string {
	srv := sshtest.Start(t, sshtest.Options{User: "deploy", Password: "pw"})
	return writeConfig(t, fmt.Sprintf(`defaults:
  user: deploy
  host_key_policy: insecure
commands:
  - echo one
  - echo two
hosts:
  - {name: good1, host: %[1]s, port: %[2]d, group: lab, pass: pw}
  - {name: good2, host: %[1]s, port: %[2]d, group: lab, pass: pw}
  - {name: badpw, host: %[1]s, port: %[2]d, group: other, pass: wrong}
`, srv.Host, srv.Port))
}

func TestRunParallelAllOK(t *testing.T) {
	cfg := serverConfig(t)
	out, err := execRoot(t, "-f", cfg, "-g", "lab", "-p", "2", "run", "-c", "echo hello")
	if err != nil || exitCode(err) != 0 {
		t.Fatalf("err = %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "good1") || !strings.HasPrefix(lines[2], "good2") {
		t.Fatalf("unexpected table:\n%s", out)
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "hello") || !strings.Contains(l, " 0 ") {
			t.Errorf("row %q", l)
		}
	}
}

func TestRunHostFailureExitCode(t *testing.T) {
	cfg := serverConfig(t)
	out, err := execRoot(t, "-f", cfg, "-H", "good1,badpw", "run", "-c", "echo hi")
	if exitCode(err) != exitHostFailed {
		t.Fatalf("exit code = %d (err %v)\n%s", exitCode(err), err, out)
	}
	if !strings.Contains(out, "good1") || !strings.Contains(out, "[error]") || !strings.Contains(err.Error(), "1 of 2 hosts failed") {
		t.Errorf("output:\n%s\nerr: %v", out, err)
	}

	// A non-zero remote exit also fails the run.
	_, err = execRoot(t, "-f", cfg, "-H", "good1", "run", "-c", "exit 3")
	if exitCode(err) != exitHostFailed {
		t.Errorf("exit 3: exit code = %d", exitCode(err))
	}
}

func TestRunConfigCommandsAndScript(t *testing.T) {
	cfg := serverConfig(t)
	out, err := execRoot(t, "-f", cfg, "-H", "good1", "run")
	if err != nil || !strings.Contains(out, "one (+1 lines)") {
		t.Errorf("commands: err=%v\n%s", err, out)
	}

	script := filepath.Join(t.TempDir(), "s.sh")
	_ = os.WriteFile(script, []byte("echo from-script\n"), 0o600)
	out, err = execRoot(t, "-f", cfg, "-H", "good1", "run", "--script", script)
	if err != nil || !strings.Contains(out, "from-script") {
		t.Errorf("script: err=%v\n%s", err, out)
	}
}

func TestRunDryRunMakesNoConnection(t *testing.T) {
	// Secrets that cannot resolve prove dry-run skips resolution too.
	cfg := writeConfig(t, "hosts:\n  - {name: far, host: 192.0.2.1, pass_env: MRSH_TEST_UNSET_VAR}\n")
	start := time.Now()
	out, err := execRoot(t, "-f", cfg, "--dry-run", "-H", "far,root@192.0.2.2:2222", "run", "-c", "uptime")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("dry-run took too long; did it connect?")
	}
	for _, want := range []string{"would run on 2 host(s)", "192.0.2.1:22", "192.0.2.2:2222", "literal", "uptime"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: a, host: 192.0.2.1}\n")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"run"}, "nothing to run"},
		{[]string{"run", "-c", "x", "--script", "y"}, "none of the others can be"},
		{[]string{"-g", "nope", "run", "-c", "x"}, "no target hosts"},
		{[]string{"--host-key-policy", "yolo", "run", "-c", "x"}, "invalid --host-key-policy"},
	}
	for _, tt := range tests {
		_, err := execRoot(t, append([]string{"-f", cfg}, tt.args...)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) || exitCode(err) != exitUsage {
			t.Errorf("%v: err = %v (exit %d), want %q", tt.args, err, exitCode(err), tt.want)
		}
	}
}

func TestRunResolveFailureStopsBeforeConnecting(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: a, host: 192.0.2.1, pass_env: MRSH_TEST_UNSET_VAR}\n")
	_, err := execRoot(t, "-f", cfg, "run", "-c", "x")
	if err == nil || !strings.Contains(err.Error(), "MRSH_TEST_UNSET_VAR is unset") || exitCode(err) != exitUsage {
		t.Errorf("err = %v", err)
	}
}

func TestRunInterrupted(t *testing.T) {
	cfg := serverConfig(t)
	root := newRootCmd()
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"-f", cfg, "-H", "good1", "run", "-c", "sleep 30"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := root.ExecuteContext(ctx)
	if exitCode(err) != exitInterrupted {
		t.Errorf("exit code = %d (err %v)", exitCode(err), err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("interrupt did not stop the remote command promptly")
	}
}
