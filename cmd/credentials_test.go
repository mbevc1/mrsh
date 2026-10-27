package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/sshtest"
)

func TestCredentialsLifecycle(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: rt1, host: 10.0.0.1}\n  - {name: rt2, host: 10.0.0.2}\n")

	if out, err := execRoot(t, "-f", cfg, "credentials", "set", "--name", "lab", "-u", "admin", "--pass-env", "LAB_PASS", "-i", "~/.ssh/lab"); err != nil || !strings.Contains(out, "added credential lab") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if out, err := execRoot(t, "-f", cfg, "creds", "set", "--name", "lab", "--pass-arn", "arn:aws:ssm:eu-west-1:123456789012:parameter/lab"); err != nil || !strings.Contains(out, "updated credential lab") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if _, err := execRoot(t, "-f", cfg, "credentials", "set", "--name", "lab"); err == nil {
		t.Error("set with no fields succeeded")
	}
	if _, err := execRoot(t, "-f", cfg, "hosts", "update", "--name", "rt1", "--credential", "nope"); err == nil || !strings.Contains(err.Error(), `credential "nope" is not defined`) {
		t.Errorf("unknown set accepted: %v", err)
	}
	if _, err := execRoot(t, "-f", cfg, "hosts", "update", "--name", "rt1", "--credential", "lab"); err != nil {
		t.Fatal(err)
	}

	// rt1 takes user, pass and key from the set; rt2 has none.
	out, err := execRoot(t, "-f", cfg, "-o", "json", "hosts", "list")
	if err != nil {
		t.Fatal(err)
	}
	var rows []hostRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if r := rows[0]; r.Credential != "lab" || r.User != "admin" || !strings.HasPrefix(r.Pass, "arn:aws:ssm") || !strings.HasSuffix(r.IdentityFile, ".ssh/lab") {
		t.Errorf("rt1 = %+v", r)
	}
	if r := rows[1]; r.Credential != "" || r.User != "" {
		t.Errorf("rt2 = %+v", r)
	}

	out, err = execRoot(t, "-f", cfg, "credentials", "list")
	if err != nil || !strings.Contains(out, "USED BY") || !strings.Contains(out, "rt1") {
		t.Errorf("list: %v\n%s", err, out)
	}

	// In use: refused, naming the users.
	if _, err := execRoot(t, "-f", cfg, "-y", "credentials", "remove", "--name", "lab"); err == nil || !strings.Contains(err.Error(), "used by rt1") {
		t.Errorf("remove in use: %v", err)
	}
	if _, err := execRoot(t, "-f", cfg, "hosts", "update", "--name", "rt1", "--credential", ""); err != nil {
		t.Fatal(err)
	}
	if out, err := execRoot(t, "-f", cfg, "-y", "credentials", "remove", "--name", "lab"); err != nil || !strings.Contains(out, "removed credential lab") {
		t.Errorf("remove: %v\n%s", err, out)
	}
	if body := readFile(t, cfg); strings.Contains(body, "credentials") || strings.Contains(body, "credential:") {
		t.Errorf("leftovers:\n%s", body)
	}
}

// A host with no user or pass of its own logs in with its credential set's.
func TestRunUsesCredentialSet(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{User: "deploy", Password: "pw"})
	t.Setenv("MRSH_TEST_SET_PASS", "pw")
	cfg := writeConfig(t, fmt.Sprintf(`defaults: {host_key_policy: insecure, user: nobody, pass: wrong}
credentials:
  lab: {user: deploy, pass_env: MRSH_TEST_SET_PASS}
hosts:
  - {name: viaset, host: %[1]s, port: %[2]d, credential: lab}
  - {name: override, host: %[1]s, port: %[2]d, credential: lab, pass: wrong}
`, srv.Host, srv.Port))
	out, err := execRoot(t, "-f", cfg, "-o", "json", "run", "-c", "echo hi")
	var recs []map[string]any
	if jerr := json.Unmarshal([]byte(out), &recs); jerr != nil || len(recs) != 2 {
		t.Fatalf("output: %v\n%s", jerr, out)
	}
	if recs[0]["name"] != "viaset" || recs[0]["exit_code"] != float64(0) || recs[0]["stdout"] != "hi\n" {
		t.Errorf("viaset = %v", recs[0])
	}
	if recs[1]["exit_code"] == float64(0) {
		t.Errorf("host pass should override the set's: %v", recs[1])
	}
	if exitCode(err) != exitHostFailed {
		t.Errorf("exit = %d (%v)", exitCode(err), err)
	}
}
