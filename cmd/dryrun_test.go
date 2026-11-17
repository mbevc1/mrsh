package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/ssmtest"
)

func TestUnifiedDiff(t *testing.T) {
	a := strings.Split("a b c d e f g h i j k l m", " ")
	b := strings.Split("a B c d e f g h i j k l m n", " ")
	got := unifiedDiff("x", a, b)
	want := "--- x\n+++ x (after)\n" +
		"@@ -1,5 +1,5 @@\n a\n-b\n+B\n c\n d\n e\n" +
		"@@ -11,3 +11,4 @@\n k\n l\n m\n+n\n"
	if got != want {
		t.Errorf("diff:\n%s\nwant:\n%s", got, want)
	}
	if got := unifiedDiff("x", nil, []string{"new"}); !strings.Contains(got, "@@ -0,0 +1,1 @@\n+new\n") {
		t.Errorf("from nothing:\n%s", got)
	}
}

func TestMaskPasswords(t *testing.T) {
	before := "defaults: {pass: d0}\n  - {name: a, pass: 's3 cret', pass_env: KEEP}\n    pass: \"q\\\"x\"  # note\n    pass_arn: arn:aws:ssm:x\n"
	after := "    pass: s3cret2\n    pass: d0\n"
	m := maskPasswords([]byte(before), []byte(after))
	for _, secret := range []string{"d0", "s3 cret", `q\"x`, "s3cret2"} {
		if strings.Contains(m[0]+m[1], secret) {
			t.Errorf("%q not masked:\n%s\n%s", secret, m[0], m[1])
		}
	}
	if !strings.Contains(m[0], "pass_env: KEEP") || !strings.Contains(m[0], "pass_arn: arn:aws:ssm:x") || !strings.Contains(m[0], "  # note") {
		t.Errorf("masked too much:\n%s", m[0])
	}
	// The same value keeps its number across both texts; a new one gets the next.
	if !strings.Contains(m[0], "{pass: <password #1>}") || !strings.Contains(m[1], "pass: <password #4>\n    pass: <password #1>") {
		t.Errorf("numbering:\n%s\n%s", m[0], m[1])
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	fake := ssmtest.Start(t)
	shared := fake.Put("/mrsh/hosts/lab/pass", "pw")
	cfg := writeConfig(t, "# keep me\ncredentials:\n  routers: {user: admin}\nhosts:\n"+
		"  - {name: web01, host: 10.0.0.1, pass: s3cret}\n"+
		"  - {name: lab1, host: 10.0.0.5, group: lab, pass_arn: "+shared+"}\n  - {name: lab2, host: 10.0.0.6, group: lab, pass_arn: "+shared+"}\n")
	before := readFile(t, cfg)
	dry := func(args ...string) string {
		t.Helper()
		// Empty stdin: a confirmation prompt would abort, so none may be asked.
		out, _, err := execIn(t, "", append([]string{"-q", "-f", cfg, "--dry-run"}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if readFile(t, cfg) != before {
			t.Fatalf("%v wrote the config", args)
		}
		return out
	}

	out := dry("hosts", "update", "--name", "web01", "--port", "2222")
	if !strings.Contains(out, "(nothing written)") || !strings.Contains(out, "+    port: 2222") || strings.Contains(out, "s3cret") || strings.Contains(out, "updated host") {
		t.Errorf("update:\n%s", out)
	}
	if out := dry("hosts", "update", "--in-group", "lab", "-g", "staging"); strings.Count(out, "+    group: staging") != 2 {
		t.Errorf("bulk update:\n%s", out)
	}
	out = dry("hosts", "remove", "-g", "lab", "--delete-secret")
	if !strings.Contains(out, "-  - {name: lab1") || !strings.Contains(out, "dry-run: would delete SSM parameter /mrsh/hosts/lab/pass") || strings.Contains(out, "removed") {
		t.Errorf("remove:\n%s", out)
	}
	out = dry("creds", "set", "--name", "lab", "-u", "ops", "--pass-ssm")
	if !strings.Contains(out, "would ask for the pass and store it for creds lab in SSM as /mrsh/creds/lab/pass") || !strings.Contains(out, "+    pass_arn: arn:aws:ssm:region:000000000000:parameter/mrsh/creds/lab/pass") {
		t.Errorf("pass-ssm:\n%s", out)
	}
	for _, args := range [][]string{
		{"hosts", "add", "--name", "web09", "-H", "10.0.0.9"},
		{"creds", "rename", "--name", "routers", "--to", "core"},
		{"creds", "default", "routers"},
		{"creds", "assign", "--name", "routers", "-g", "lab"},
		{"creds", "remove", "--name", "routers"},
	} {
		if out := dry(args...); !strings.Contains(out, "(nothing written)") {
			t.Errorf("%v:\n%s", args, out)
		}
	}
	if len(fake.Calls()) != 0 || fake.Param("/mrsh/hosts/lab/pass") == nil {
		t.Errorf("AWS was called: %v", fake.Calls())
	}

	// init and copy show the file they would create.
	dir := t.TempDir()
	if out, _, err := execIn(t, "", "-q", "-f", filepath.Join(dir, "new.yaml"), "--dry-run", "hosts", "init"); err != nil || !strings.Contains(out, "@@ -0,0 +1,") {
		t.Errorf("init: %v\n%s", err, out)
	}
	if out, _, err := execIn(t, "", "-q", "-f", cfg, "--dry-run", "hosts", "copy", "--to", filepath.Join(dir, "copy.yaml")); err != nil || !strings.Contains(out, "+# keep me") {
		t.Errorf("copy: %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}

	// A refused edit is still refused, and the UI has no dry run.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "--dry-run", "hosts", "update", "--name", "ghost", "--port", "1"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("unknown host: %v", err)
	}
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "--dry-run", "ui", "--no-open"); err == nil || !strings.Contains(err.Error(), "does not apply to mrsh ui") {
		t.Errorf("ui: %v", err)
	}
}
