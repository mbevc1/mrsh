package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mbevc1/mrsh/internal/config"
)

const testdataHosts = "../testdata/hosts.yaml"

func TestHostsListMasksSecrets(t *testing.T) {
	out, err := execRoot(t, "hosts", "list", "-f", testdataHosts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "literalsecret") {
		t.Errorf("literal password not masked:\n%s", out)
	}
	if !strings.Contains(out, "deploy") {
		t.Errorf("username hidden; usernames are shown without --show-secrets:\n%s", out)
	}
	for _, want := range []string{config.Mask, "env:WEB01_PASS", "arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/db02/pass", "2222"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestHostsListShowSecrets(t *testing.T) {
	out, err := execRoot(t, "hosts", "list", "-f", testdataHosts, "--show-secrets", "-g", "web", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []hostRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[1].Name != "web02" || rows[1].Pass != "literalsecret" || rows[1].User != "deploy" {
		t.Errorf("rows = %+v", rows)
	}
	if rows[0].Pass != "env:WEB01_PASS" {
		t.Errorf("reference was resolved or hidden: %+v", rows[0])
	}
}

func TestHostsListHostFilter(t *testing.T) {
	out, err := execRoot(t, "hosts", "list", "-f", testdataHosts, "-H", "db02,10.0.0.1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []hostRow
	_ = json.Unmarshal([]byte(out), &rows)
	if len(rows) != 2 || rows[0].Name != "web01" || rows[1].Name != "db02" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestHostsListInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	_ = os.WriteFile(path, []byte("hosts:\n  - {name: a, host: h, pass: x, pass_env: X}\n"), 0o600)
	_, err := execRoot(t, "hosts", "list", "-f", path)
	if err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Errorf("err = %v", err)
	}
}

func TestHostsInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if _, err := execRoot(t, "hosts", "init", "-f", path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := execRoot(t, "hosts", "init", "-f", path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second init err = %v", err)
	}
	_ = os.WriteFile(path, []byte("changed"), 0o600)
	if _, err := execRoot(t, "hosts", "init", "-f", path, "--force"); err != nil {
		t.Fatalf("init --force: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != starterConfig {
		t.Errorf("--force did not overwrite")
	}
}

// TestDefaultsPrecedence checks CLI flag > MRSH_DEBUG > hosts.yaml defaults > built-in.
func TestDefaultsPrecedence(t *testing.T) {
	dir := t.TempDir()
	withDefaults := filepath.Join(dir, "with.yaml")
	_ = os.WriteFile(withDefaults, []byte("defaults: {parallel: 5, timeout: 9, output: json}\nhosts: []\n"), 0o600)
	without := filepath.Join(dir, "without.yaml")
	_ = os.WriteFile(without, []byte("hosts: []\n"), 0o600)

	tests := []struct {
		name     string
		args     []string
		env      string
		parallel int
		timeout  int
		output   string
		debug    bool
	}{
		{"built-in", []string{"-f", without}, "", 1, 30, "text", false},
		{"config defaults", []string{"-f", withDefaults}, "", 5, 9, "json", false},
		{"flags win", []string{"-f", withDefaults, "-p", "2", "-t", "3", "-o", "csv"}, "", 2, 3, "csv", false},
		{"explicit flag equal to built-in wins", []string{"-f", withDefaults, "-p", "1"}, "", 1, 9, "json", false},
		{"env debug", []string{"-f", without}, "1", 1, 30, "text", true},
		{"flag debug", []string{"-f", without, "-d"}, "", 1, 30, "text", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MRSH_DEBUG", tt.env)
			root, opts := newRootCmdWithOptions()
			var got globalOptions
			root.AddCommand(&cobra.Command{
				Use: "probe",
				RunE: func(cmd *cobra.Command, _ []string) error {
					if _, err := loadConfig(cmd, opts); err != nil {
						return err
					}
					got = *opts
					return nil
				},
			})
			root.SetOut(&strings.Builder{})
			root.SetErr(&strings.Builder{})
			root.SetArgs(append(tt.args, "probe"))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.parallel != tt.parallel || got.timeout != tt.timeout || got.output != tt.output || got.debug != tt.debug {
				t.Errorf("got parallel=%d timeout=%d output=%s debug=%v", got.parallel, got.timeout, got.output, got.debug)
			}
		})
	}
}

func TestAliasesRunEndToEnd(t *testing.T) {
	out, err := execRoot(t, "h", "list", "-f", testdataHosts, "-g", "db")
	if err != nil || !strings.Contains(out, "db02") || strings.Contains(out, "web01") {
		t.Errorf("mrsh h list: err=%v\n%s", err, out)
	}
	out, err = execRoot(t, "-f", testdataHosts, "--dry-run", "-H", "web01", "r", "-c", "uptime")
	if err != nil || !strings.Contains(out, "would run on 1 host") {
		t.Errorf("mrsh r: err=%v\n%s", err, out)
	}
}

func TestSubcommandAliases(t *testing.T) {
	root := newRootCmd()
	for alias, want := range map[string]string{
		"host ls": "hosts list", "ho l": "hosts list", "h i": "hosts init", "h ex": "hosts example", "h a": "hosts add",
		"h u": "hosts update", "h up": "hosts update", "h r": "hosts remove", "h rm": "hosts remove",
		"creds ls": "credentials list", "creds l": "credentials list", "creds s": "credentials set",
		"creds r": "credentials remove", "creds rm": "credentials remove", "creds ass": "credentials assign",
		"mt b": "mt backup", "mt ba": "mt backup", "mt bck": "mt backup", "mt up": "mt upgrade", "mt re": "mt reboot", "mt v": "mt version", "mt ver": "mt version",
	} {
		c, _, err := root.Find(strings.Fields(alias))
		if got := strings.TrimPrefix(c.CommandPath(), "mrsh "); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", alias, got, err, want)
		}
	}
}

func TestHostsExample(t *testing.T) {
	out, err := execRoot(t, "hosts", "example")
	if err != nil {
		t.Fatal(err)
	}
	if out != string(config.Example) {
		t.Fatalf("output differs from config.Example:\n%s", out)
	}
}

func TestHostsCopy(t *testing.T) {
	src := writeConfig(t, "# team routers\ndefaults: {user: admin}\nhosts:\n  - {name: rt1, host: 10.0.0.1, pass_env: RT1_PASS} # core\n")
	dst := filepath.Join(t.TempDir(), "moved", "hosts.yaml")

	out, errOut, err := runSplit(t, "-f", src, "hosts", "cp", "--to", dst)
	if err != nil || !strings.Contains(out, "copied "+src+" to "+dst) || !strings.Contains(out, "export MRSH_CONFIG="+dst) || errOut != "" {
		t.Fatalf("copy: err=%v\nout:%s\nerr:%s", err, out, errOut)
	}
	if got, want := readFile(t, dst), readFile(t, src); got != want {
		t.Errorf("copy is not byte for byte:\n%s\nwant:\n%s", got, want)
	}

	// An existing destination is kept unless --force.
	_ = os.WriteFile(src, []byte("hosts:\n  - {name: rt2, host: 10.0.0.2}\n"), 0o600)
	if _, err := execRoot(t, "-f", src, "hosts", "copy", "--to", dst); err == nil || !strings.Contains(err.Error(), "already exists (use --force") {
		t.Errorf("no-clobber: %v", err)
	}
	if !strings.Contains(readFile(t, dst), "rt1") {
		t.Error("destination changed without --force")
	}
	if _, err := execRoot(t, "-f", src, "hosts", "copy", "--to", dst, "--force"); err != nil || readFile(t, dst) != readFile(t, src) {
		t.Errorf("--force: %v", err)
	}

	// An invalid source is never copied; nor is a copy onto itself.
	bad := writeConfig(t, "hosts:\n  - {name: x}\n")
	other := filepath.Join(t.TempDir(), "x.yaml")
	if _, err := execRoot(t, "-f", bad, "hosts", "copy", "--to", other); err == nil {
		t.Error("copied an invalid config")
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("invalid copy left a file: %v", err)
	}
	if _, err := execRoot(t, "-f", src, "hosts", "copy", "--to", src); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("self copy: %v", err)
	}
	if _, err := execRoot(t, "-f", src, "hosts", "copy"); err == nil || !strings.Contains(err.Error(), `"to" not set`) {
		t.Errorf("missing --to: %v", err)
	}
}

func TestHostsCopyWarnsAboutLiteralPasswords(t *testing.T) {
	src := writeConfig(t, "hosts:\n  - {name: rt1, host: 10.0.0.1, pass: s3cret}\n")
	dst := filepath.Join(t.TempDir(), "hosts.yaml")
	_, errOut, err := runSplit(t, "-f", src, "hosts", "copy", "--to", dst)
	if err != nil || !strings.Contains(errOut, src+" stores 1 literal password") || !strings.Contains(errOut, dst+" stores 1 literal password") {
		t.Errorf("err=%v\nstderr:%s", err, errOut)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: from-env, host: 10.0.0.9}\n")
	t.Setenv("MRSH_CONFIG", cfg)
	if out, err := execRoot(t, "hosts", "list"); err != nil || !strings.Contains(out, "from-env") {
		t.Errorf("MRSH_CONFIG: err=%v\n%s", err, out)
	}
	// -f still wins over the variable.
	other := writeConfig(t, "hosts:\n  - {name: from-flag, host: 10.0.0.8}\n")
	if out, err := execRoot(t, "-f", other, "hosts", "list"); err != nil || !strings.Contains(out, "from-flag") || strings.Contains(out, "from-env") {
		t.Errorf("-f over MRSH_CONFIG: err=%v\n%s", err, out)
	}
}
