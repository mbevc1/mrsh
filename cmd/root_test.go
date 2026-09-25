package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// execRoot runs a fresh root command with args and returns its stdout.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestHostAliasesAccumulate(t *testing.T) {
	root := newRootCmd()
	var got []string
	root.AddCommand(&cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			got, _ = cmd.Flags().GetStringSlice("host")
			return nil
		},
	})
	root.SetArgs([]string{"-H", "web01", "--hosts", "web02,web03", "--host", "10.0.0.5", "probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := []string{"web01", "web02", "web03", "10.0.0.5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
}

func TestLowercaseHIsHelp(t *testing.T) {
	out, err := execRoot(t, "-h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "-H, --host") {
		t.Errorf("help output missing expected content:\n%s", out)
	}
}

func TestInvalidOutputRejected(t *testing.T) {
	if _, err := execRoot(t, "version", "-o", "yaml"); err == nil {
		t.Fatal("expected error for invalid --output")
	}
}

func TestGlobalFlagDefaults(t *testing.T) {
	pf := newRootCmd().PersistentFlags()
	want := map[string]string{
		"config": "hosts.yaml", "group": "", "user": "", "identity-file": "",
		"parallel": "1", "timeout": "30", "output": "text", "dry-run": "false", "debug": "false",
	}
	for name, def := range want {
		f := pf.Lookup(name)
		if f == nil {
			t.Errorf("flag --%s not registered", name)
			continue
		}
		if f.DefValue != def {
			t.Errorf("--%s default = %q, want %q", name, f.DefValue, def)
		}
	}
	shorts := map[string]string{"config": "f", "group": "g", "host": "H", "user": "u",
		"identity-file": "i", "parallel": "p", "timeout": "t", "output": "o", "debug": "d"}
	for name, sh := range shorts {
		if f := pf.Lookup(name); f == nil || f.Shorthand != sh {
			t.Errorf("--%s shorthand want -%s", name, sh)
		}
	}
}

func TestDebugLogging(t *testing.T) {
	t.Setenv("MRSH_DEBUG", "")
	if envDebug() {
		t.Error("envDebug true with empty MRSH_DEBUG")
	}
	t.Setenv("MRSH_DEBUG", "1")
	if !envDebug() {
		t.Error("envDebug false with MRSH_DEBUG=1")
	}
}
