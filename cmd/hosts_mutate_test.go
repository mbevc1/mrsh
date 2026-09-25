package cmd

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
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
		{[]string{"hosts", "remove", "--name", "nope", "--confirm"}, "host not found"},
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

	// remove --confirm deletes the right entry.
	if _, err := execRoot(t, "-f", path, "hosts", "remove", "--name", "web01", "--confirm"); err != nil {
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
