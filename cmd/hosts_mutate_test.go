package cmd

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/ssmtest"
)

func loadFile(t *testing.T, path string) *config.Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHostsAddUpdateRemove(t *testing.T) {
	path := writeConfig(t, "# keep me\ndefaults:\n  user: deploy\nhosts: []\n")

	if _, err := execRoot(t, "-f", path, "hosts", "add", "--name", "web03", "-H", "10.0.0.3", "-g", "web",
		"--port", "2222", "-i", "~/.ssh/web", "--pass-env", "WEB03_PASS"); err != nil {
		t.Fatal(err)
	}
	c := loadFile(t, path)
	h := c.Hosts[0]
	if h.Name != "web03" || h.Host != "10.0.0.3" || h.Group != "web" || h.Port != 2222 || h.IdentityFile != "~/.ssh/web" || h.PassEnv != "WEB03_PASS" {
		t.Fatalf("added host = %+v", h)
	}

	// update touches only supplied fields; a new pass variant replaces the group.
	if _, err := execRoot(t, "-f", path, "hosts", "update", "--name", "web03", "--pass-arn",
		"arn:aws:ssm:eu-west-1:123456789012:parameter/web03", "-u", "admin"); err != nil {
		t.Fatal(err)
	}
	h = loadFile(t, path).Hosts[0]
	if h.PassEnv != "" || h.PassARN == "" || h.User != "admin" || h.Group != "web" || h.Port != 2222 || h.Host != "10.0.0.3" {
		t.Fatalf("updated host = %+v", h)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "# keep me") {
		t.Errorf("comment lost:\n%s", raw)
	}

	// remove asks first; "n" (or EOF) aborts.
	root := newRootCmd()
	root.SetIn(strings.NewReader("n\n"))
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"-f", path, "hosts", "remove", "--name", "web03"})
	if err := root.Execute(); err == nil || err.Error() != "aborted" {
		t.Errorf("declined remove err = %v", err)
	}
	if len(loadFile(t, path).Hosts) != 1 {
		t.Fatal("host removed without confirmation")
	}

	root = newRootCmd()
	root.SetIn(strings.NewReader("yes\n"))
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	root.SetArgs([]string{"-f", path, "hosts", "remove", "--name", "web03"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(loadFile(t, path).Hosts) != 0 {
		t.Error("confirmed remove did not delete")
	}
}

func TestHostsMutationErrors(t *testing.T) {
	path := writeConfig(t, "hosts:\n  - {name: web01, host: 10.0.0.1}\n  - {name: web02, host: 10.0.0.2}\n")
	before, _ := os.ReadFile(path)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"hosts", "add", "--name", "web01", "-H", "10.0.0.9"}, "already exists"},
		{[]string{"hosts", "add", "--name", "x"}, "needs the address"},
		{[]string{"hosts", "add", "--name", "x", "-H", "a,b"}, "exactly one address"},
		{[]string{"hosts", "add", "-H", "10.0.0.9"}, `"name" not set`},
		{[]string{"hosts", "add", "--name", "x", "-H", "h", "--pass", "p", "--pass-env", "P"}, "none of the others"},
		{[]string{"hosts", "add", "--name", "x", "-H", "h", "-u", "a", "--user-env", "U"}, "set only one of"},
		{[]string{"hosts", "add", "--name", "x", "-H", "h", "--pass-arn", "arn:aws:s3:::b"}, "pass_arn"},
		{[]string{"hosts", "add", "--name", "x", "-H", "h", "--port", "70000"}, "out of range"},
		{[]string{"hosts", "update", "--name", "nope", "-g", "x"}, "host not found"},
		{[]string{"hosts", "update", "--name", "web01"}, "nothing to update"},
		{[]string{"hosts", "remove", "--name", "nope", "--assume-yes"}, "host not found"},
	}
	for _, tt := range tests {
		_, err := execRoot(t, append([]string{"-f", path}, tt.args...)...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: err = %v, want %q", tt.args, err, tt.want)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("failed mutations changed the file:\n%s", after)
	}

	// remove --assume-yes deletes the right entry.
	if _, err := execRoot(t, "-f", path, "hosts", "remove", "--name", "web01", "--assume-yes"); err != nil {
		t.Fatal(err)
	}
	if c := loadFile(t, path); len(c.Hosts) != 1 || c.Hosts[0].Name != "web02" {
		t.Errorf("hosts = %+v", c.Hosts)
	}
}

func TestHostsListCSV(t *testing.T) {
	out, err := execRoot(t, "hosts", "list", "-f", testdataHosts, "-o", "csv", "-g", "db")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil || len(rows) != 3 || rows[0][0] != "name" || rows[2][2] != "2222" {
		t.Errorf("rows=%q err=%v", rows, err)
	}
}

func TestRunJSONStdoutCleanWithDebug(t *testing.T) {
	cfg := serverConfig(t)
	root := newRootCmd()
	var out, errOut strings.Builder
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"-f", cfg, "-d", "-o", "json", "-g", "lab", "run", "-c", "printf 'a\\nb'"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := jsonUnmarshal(out.String(), &recs); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, out.String())
	}
	if len(recs) != 2 || recs[0]["stdout"] != "a\nb" {
		t.Errorf("records = %v", recs)
	}
	if !strings.Contains(errOut.String(), "level=DEBUG") {
		t.Error("debug output missing from stderr")
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func TestHostsUpdateAndRemoveSeveral(t *testing.T) {
	fake := ssmtest.Start(t)
	shared := fake.Put("/mrsh/shared/pass", "pw")
	cfg := writeConfig(t, "credentials:\n  routers: {user: admin}\nhosts:\n"+
		"  - {name: web01, host: 10.0.0.1, group: web}\n  - {name: web02, host: 10.0.0.2, group: web}\n  - {name: db01, host: 10.0.0.3, group: db}\n"+
		"  - {name: lab1, host: 10.0.0.4, group: lab, pass_arn: "+shared+"}\n  - {name: lab2, host: 10.0.0.5, group: lab, pass_arn: "+shared+"}\n")
	run := func(stdin string, args ...string) (string, string, error) {
		t.Helper()
		return execIn(t, stdin, append([]string{"-q", "-f", cfg}, args...)...)
	}
	port := func(name string) int { h, _ := findHost(loadCfg(t, cfg), name); return h.Port }

	// Several hosts are listed and confirmed first.
	if _, errOut, err := run("n\n", "hosts", "update", "--name", "web01,db01", "--port", "2222"); err == nil || !strings.Contains(errOut, "About to update 2 host(s):\n  web01 (10.0.0.1)\n  db01 (10.0.0.3)") || port("web01") != 0 {
		t.Fatalf("declined: %v\n%s", err, errOut)
	}
	if out, _, err := run("y\n", "hosts", "update", "--name", "web01", "--name", "db01", "--port", "2222", "--credential", "routers"); err != nil ||
		!strings.Contains(out, "updated 2 host(s)") || !strings.HasSuffix(strings.TrimSpace(out), ": web01, db01") || port("web01") != 2222 || port("db01") != 2222 || port("web02") != 0 {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if _, _, err := run("", "hosts", "update", "--in-group", "web", "-g", "staging", "-y"); err != nil {
		t.Fatal(err)
	}
	for _, h := range loadCfg(t, cfg).Hosts[:2] {
		if h.Group != "staging" {
			t.Errorf("%s group = %q", h.Name, h.Group)
		}
	}

	// What applies to one host only is refused for several.
	for args, want := range map[string]string{
		"hosts update --name web01,web02 -H 10.9.9.9 -y":   "sets one host's address",
		"hosts update --name web01,web02 --pass-ssm -y":    "store it in a credential set",
		"hosts update --name web01,web02 --rotate-pass -y": "one host at a time",
		"hosts update --name web01,web02 -y":               "nothing to update",
		"hosts update --name web01,ghost --port 1 -y":      "ghost",
		"hosts update --in-group nosuch --port 1 -y":       "no hosts in group nosuch",
		"hosts update --port 1 -y":                         "needs --name",
		"hosts remove -y":                                  "needs --name",
	} {
		if _, _, err := run("", strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}

	// Removing several at once; a parameter only they share goes with them.
	if out, _, err := run("", "hosts", "remove", "--name", "lab1,lab2", "--delete-secret", "-y"); err != nil ||
		!strings.Contains(out, "removed 2 hosts (lab1, lab2)") || !strings.Contains(out, "deleted SSM parameter /mrsh/shared/pass") || fake.Param("/mrsh/shared/pass") != nil {
		t.Fatalf("remove several: %v\n%s", err, out)
	}
	if out, _, err := run("", "-g", "staging", "hosts", "remove", "-y"); err != nil || !strings.Contains(out, "removed 2 hosts (web01, web02)") {
		t.Fatalf("remove by group: %v\n%s", err, out)
	}
	if hosts := loadCfg(t, cfg).Hosts; len(hosts) != 1 || hosts[0].Name != "db01" {
		t.Errorf("left: %+v", hosts)
	}
}
