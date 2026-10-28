package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
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

func TestCredentialsDefaultAssignRename(t *testing.T) {
	cfg := writeConfig(t, `# keep me
defaults:
  port: 22 # ssh
credentials:
  lab: {user: admin, pass_env: LAB_PASS}
hosts:
  - {name: rt1, host: 10.0.0.1, group: routers}
  - {name: rt2, host: 10.0.0.2, group: routers}
  - {name: web, host: 10.0.0.3, group: web}
`)
	run := func(args ...string) string {
		t.Helper()
		out, err := execRoot(t, append([]string{"-f", cfg}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}

	// default shows "-" when unset, then sets it (also via the creds alias).
	if out := run("credentials", "default"); strings.TrimSpace(out) != "-" {
		t.Errorf("default (unset) = %q", out)
	}
	run("creds", "default", "lab")
	if out := run("credentials", "default"); strings.TrimSpace(out) != "lab" {
		t.Errorf("default = %q", out)
	}
	if body := readFile(t, cfg); !strings.Contains(body, "# keep me") || !strings.Contains(body, "port: 22 # ssh") {
		t.Errorf("defaults comments lost:\n%s", body)
	}
	if _, err := execRoot(t, "-f", cfg, "credentials", "default", "nope"); err == nil {
		t.Error("default to an unknown set accepted")
	}

	// assign by group, then clear by name.
	if out := run("credentials", "assign", "--name", "lab", "-g", "routers"); !strings.Contains(out, "to 2 host(s)") || !strings.Contains(out, "rt1, rt2") {
		t.Errorf("assign: %s", out)
	}
	if out := run("credentials", "assign", "--name", "lab", "-g", "routers"); !strings.Contains(out, "nothing to change") {
		t.Errorf("assign again: %s", out)
	}
	if out := run("credentials", "assign", "--clear", "-H", "rt2"); !strings.Contains(out, "cleared the credential set on 1 host(s)") {
		t.Errorf("clear: %s", out)
	}
	for _, args := range [][]string{
		{"credentials", "assign", "--name", "lab"},                         // no selection
		{"credentials", "assign", "-g", "routers"},                         // no --name/--clear
		{"credentials", "assign", "--name", "lab", "--clear", "-H", "rt1"}, // both
		{"credentials", "assign", "--name", "lab", "-H", "10.9.9.9"},       // literal address
	} {
		if _, err := execRoot(t, append([]string{"-f", cfg}, args...)...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}

	// rename moves the set and every reference in one save.
	if out := run("credentials", "rename", "--name", "lab", "--to", "routers"); !strings.Contains(out, "updated defaults, rt1") {
		t.Errorf("rename: %s", out)
	}
	c := loadTestConfig(t, cfg)
	if _, ok := c.Credentials["routers"]; !ok || len(c.Credentials) != 1 || c.Defaults.Credential != "routers" ||
		c.Hosts[0].Credential != "routers" || c.Hosts[1].Credential != "" {
		t.Errorf("after rename: defaults=%q hosts=%+v sets=%v", c.Defaults.Credential, c.Hosts, c.Credentials)
	}
	if _, err := execRoot(t, "-f", cfg, "credentials", "rename", "--name", "nope", "--to", "x"); err == nil {
		t.Error("rename of a missing set accepted")
	}

	// Clearing the default.
	run("credentials", "default", "--clear")
	if c := loadTestConfig(t, cfg); c.Defaults.Credential != "" || c.Defaults.Port != 22 {
		t.Errorf("after clear: %+v", c.Defaults)
	}
}

func loadTestConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCredentialsAliases(t *testing.T) {
	root := newRootCmd()
	for _, alias := range []string{"creds", "cred"} {
		if c, _, err := root.Find([]string{alias}); err != nil || c.Name() != "credentials" {
			t.Errorf("%s -> %v, %v", alias, c, err)
		}
	}
	for _, gone := range []string{"identities", "identity", "ids"} {
		if c, _, _ := root.Find([]string{gone}); c != nil && c.Name() == "credentials" {
			t.Errorf("%s still resolves to credentials", gone)
		}
	}
}
