package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/sshtest"
	"github.com/mbevc1/mrsh/internal/ssmtest"
)

// execIn runs mrsh with stdin, returning stdout and stderr separately.
func execIn(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func loadCfg(t *testing.T, path string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCredentialsSetPassSSM(t *testing.T) {
	fake := ssmtest.Start(t)
	srv := sshtest.Start(t, sshtest.Options{User: "admin", Password: "s3cret"})
	cfg := writeConfig(t, fmt.Sprintf("defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin}\nhosts:\n  - {name: box, host: %s, port: %d, credential: routers}\n", srv.Host, srv.Port))

	out, _, err := execIn(t, "s3cret\n", "-f", cfg, "creds", "set", "--name", "routers", "--pass-ssm")
	if err != nil || !strings.Contains(out, "stored the pass for creds routers in SSM as /mrsh/creds/routers/pass") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	p := fake.Param("/mrsh/creds/routers/pass")
	if p == nil || p.Value != "s3cret" || p.Type != "SecureString" || p.Tags["managed-by"] != "mrsh" {
		t.Fatalf("parameter = %+v", p)
	}
	set := loadCfg(t, cfg).Credentials["routers"]
	if set.PassARN != fake.ARN("/mrsh/creds/routers/pass") || set.Pass != "" || set.User != "admin" {
		t.Fatalf("credential = %+v", set)
	}
	if strings.Contains(readFile(t, cfg), "s3cret") {
		t.Fatal("the password reached the config file")
	}

	// The stored password logs in: SSM -> resolver -> SSH.
	if out, err := execRoot(t, "-q", "-f", cfg, "-H", "box", "run", "-c", "echo via-ssm"); err != nil || !strings.Contains(out, "via-ssm") {
		t.Errorf("run with the SSM password: %v\n%s", err, out)
	}

	// An existing parameter is kept unless --overwrite.
	if _, _, err := execIn(t, "other\n", "-f", cfg, "creds", "set", "--name", "routers", "--pass-ssm"); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Errorf("second store: %v", err)
	}
	if fake.Param("/mrsh/creds/routers/pass").Value != "s3cret" {
		t.Error("value replaced without --overwrite")
	}
	if _, _, err := execIn(t, "rotated\n", "-f", cfg, "creds", "set", "--name", "routers", "--pass-ssm", "--overwrite"); err != nil || fake.Param("/mrsh/creds/routers/pass").Value != "rotated" {
		t.Errorf("--overwrite: %v", err)
	}
}

func TestSSMFlagsUserNamesAndPrefix(t *testing.T) {
	fake := ssmtest.Start(t)
	cfg := writeConfig(t, "defaults: {ssm_prefix: /team-a}\nhosts:\n  - {name: rb02, host: 10.0.0.2}\n")

	// -u with --user-ssm stores the user; =NAME picks the parameter.
	if _, _, err := execIn(t, "pw\n", "-f", cfg, "hosts", "update", "--name", "rb02", "-u", "ops", "--user-ssm", "--pass-ssm=/custom/rb02"); err != nil {
		t.Fatal(err)
	}
	if fake.Param("/team-a/hosts/rb02/user").Value != "ops" || fake.Param("/custom/rb02").Value != "pw" {
		t.Fatalf("params: user=%+v pass=%+v", fake.Param("/team-a/hosts/rb02/user"), fake.Param("/custom/rb02"))
	}
	h := loadCfg(t, cfg).Hosts[0]
	if h.User != "" || h.UserARN != fake.ARN("/team-a/hosts/rb02/user") || h.PassARN != fake.ARN("/custom/rb02") {
		t.Fatalf("host = %+v", h)
	}

	// The user can come from a prompt too; a new host gets the default name.
	if _, _, err := execIn(t, "deploy\nhunter2\n", "-f", cfg, "hosts", "add", "--name", "web01", "-H", "10.0.0.9", "--user-ssm", "--pass-ssm"); err != nil {
		t.Fatal(err)
	}
	if fake.Param("/team-a/hosts/web01/user").Value != "deploy" || fake.Param("/team-a/hosts/web01/pass").Value != "hunter2" {
		t.Error("prompted values not stored")
	}
}

func TestSSMFlagsRefuseBeforeStoring(t *testing.T) {
	fake := ssmtest.Start(t)
	cfg := writeConfig(t, "hosts:\n  - {name: rb02, host: 10.0.0.2}\n")
	for _, tc := range []struct {
		name, stdin, want string
		args              []string
	}{
		{"unknown host", "pw\n", "not found", []string{"hosts", "update", "--name", "ghost", "--pass-ssm"}},
		{"host exists", "pw\n", "already exists", []string{"hosts", "add", "--name", "rb02", "-H", "10.0.0.3", "--pass-ssm"}},
		{"no password", "", "no password on stdin", []string{"hosts", "update", "--name", "rb02", "--pass-ssm"}},
		{"bad name", "pw\n", "must look like", []string{"hosts", "update", "--name", "rb02", "--pass-ssm=relative/name"}},
		{"reserved name", "pw\n", "reserved", []string{"hosts", "update", "--name", "rb02", "--pass-ssm=/aws/x"}},
		{"two pass sources", "pw\n", "none of the others can be", []string{"hosts", "update", "--name", "rb02", "--pass", "x", "--pass-ssm"}},
		{"overwrite alone", "", "--overwrite only applies", []string{"hosts", "update", "--name", "rb02", "-g", "x", "--overwrite"}},
	} {
		if _, _, err := execIn(t, tc.stdin, append([]string{"-f", cfg}, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	for _, c := range fake.Calls() {
		if c == "PutParameter" {
			t.Fatalf("a refused command stored a secret: %v", fake.Calls())
		}
	}
}

func TestSSMStoredButConfigRejected(t *testing.T) {
	fake := ssmtest.Start(t)
	cfg := writeConfig(t, "hosts: []\n")
	// The address fails validation only when the config is saved, after the store.
	_, _, err := execIn(t, "pw\n", "-f", cfg, "hosts", "add", "--name", "x", "-H", "http://bad", "--pass-ssm")
	if err == nil || !strings.Contains(err.Error(), "--pass-arn "+fake.ARN("/mrsh/hosts/x/pass")) {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveDeleteSecret(t *testing.T) {
	fake := ssmtest.Start(t)
	own, shared, outside := fake.Put("/mrsh/hosts/rb02/pass", "a"), fake.Put("/mrsh/creds/lab/pass", "b"), fake.Put("/elsewhere/rb02", "c")
	cfg := writeConfig(t, fmt.Sprintf("credentials:\n  lab: {user: admin, pass_arn: %s}\n  spare: {user: x, pass_arn: %s}\n"+
		"hosts:\n  - {name: rb02, host: 10.0.0.2, pass_arn: %s, user_arn: %s}\n  - {name: rb03, host: 10.0.0.3, pass_arn: %s}\n",
		shared, fake.Put("/mrsh/creds/spare/pass", "d"), own, outside, shared))

	out, _, err := execIn(t, "", "-f", cfg, "hosts", "remove", "--name", "rb02", "--delete-secret", "-y")
	if err != nil || fake.Param("/mrsh/hosts/rb02/pass") != nil || fake.Param("/elsewhere/rb02") == nil ||
		!strings.Contains(out, "deleted SSM parameter /mrsh/hosts/rb02/pass") || !strings.Contains(out, "not under /mrsh") {
		t.Fatalf("host remove: %v\n%s", err, out)
	}

	// rb03 still uses lab's parameter; removing rb03 keeps it.
	out, _, err = execIn(t, "", "-f", cfg, "hosts", "remove", "--name", "rb03", "--delete-secret", "-y")
	if err != nil || fake.Param("/mrsh/creds/lab/pass") == nil || !strings.Contains(out, "still used by credential lab") {
		t.Fatalf("shared: %v\n%s", err, out)
	}

	// Without --delete-secret nothing in AWS changes.
	if _, _, err := execIn(t, "", "-f", cfg, "creds", "remove", "--name", "spare", "-y"); err != nil || fake.Param("/mrsh/creds/spare/pass") == nil {
		t.Fatalf("plain remove: %v", err)
	}
	if out, _, err := execIn(t, "", "-f", cfg, "creds", "remove", "--name", "lab", "--delete-secret", "-y"); err != nil || fake.Param("/mrsh/creds/lab/pass") != nil {
		t.Fatalf("creds remove: %v\n%s", err, out)
	}
}
