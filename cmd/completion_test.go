package cmd

import (
	"strings"
	"testing"
)

func TestTopLevelCommandsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"run", "hosts", "mt", "ui", "version", "completion"} {
		if c, _, err := root.Find([]string{name}); err != nil || c.Name() != name {
			t.Errorf("command %q not registered (err %v)", name, err)
		}
	}
	for alias, name := range map[string]string{"ru": "run", "r": "run", "ho": "hosts", "h": "hosts", "v": "version", "ver": "version"} {
		if c, _, err := root.Find([]string{alias}); err != nil || c.Name() != name {
			t.Errorf("alias %q does not resolve to %q (err %v)", alias, name, err)
		}
	}
	for _, path := range [][]string{
		{"hosts", "init"}, {"hosts", "list"}, {"hosts", "add"}, {"hosts", "update"}, {"hosts", "remove"},
		{"mt", "backup"}, {"mt", "reboot"}, {"mt", "upgrade"}, {"mt", "version"},
	} {
		if c, _, err := root.Find(path); err != nil || c.Name() != path[1] {
			t.Errorf("command %v not registered (err %v)", path, err)
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	for shell, marker := range map[string]string{
		"bash": "bash completion V2 for mrsh", "zsh": "#compdef mrsh",
		"fish": "fish completion for mrsh", "powershell": "Register-ArgumentCompleter",
	} {
		out, err := execRoot(t, "completion", shell)
		if err != nil || !strings.Contains(out, marker) {
			t.Errorf("%s: err=%v, missing %q", shell, err, marker)
		}
	}
	if _, err := execRoot(t, "completion", "tcsh"); err == nil {
		t.Error("unsupported shell accepted")
	}
}

func TestDynamicCompletion(t *testing.T) {
	tests := []struct {
		args []string
		want []string
		not  []string
	}{
		{[]string{"__complete", "-H", "we"}, []string{"web01\t10.0.0.1", "web02\t10.0.0.2"}, []string{"db01"}},
		{[]string{"__complete", "-H", "web01,d"}, []string{"web01,db01", "web01,db02"}, []string{"web02"}},
		{[]string{"__complete", "-g", ""}, []string{"db", "web"}, nil},
		{[]string{"__complete", "hosts", "remove", "--name", "db"}, []string{"db01", "db02"}, []string{"web01"}},
		{[]string{"__complete", "-o", ""}, []string{"text", "json", "csv"}, nil},
		{[]string{"__complete", "mt", "backup", "--format", ""}, []string{"rsc", "backup", "both"}, nil},
	}
	for _, tt := range tests {
		out, err := execRoot(t, append([]string{"-f", testdataHosts}, tt.args...)...)
		if err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		for _, w := range tt.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: missing %q in\n%s", tt.args, w, out)
			}
		}
		for _, n := range tt.not {
			if strings.Contains(out, n) {
				t.Errorf("%v: unexpected %q in\n%s", tt.args, n, out)
			}
		}
	}
	// A missing config yields no suggestions rather than an error.
	out, err := execRoot(t, "-f", "/nonexistent.yaml", "__complete", "-H", "")
	if err != nil || strings.Contains(out, "web01") {
		t.Errorf("missing config: err=%v out=%q", err, out)
	}
}
