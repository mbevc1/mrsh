package config

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"go.yaml.in/yaml/v3"
)

const commented = `# top comment
defaults:
  port: 22 # default port
hosts:
  # first host
  - name: web01
    host: 10.0.0.1
    group: web
  - name: web02
    host: 10.0.0.2
# trailing note
`

func mustDoc(t *testing.T, raw string) *Document {
	t.Helper()
	d, _, err := ParseDocument([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func reparse(t *testing.T, d *Document) (*Config, string) {
	t.Helper()
	out, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return c, string(out)
}

func TestDocumentMutationsKeepComments(t *testing.T) {
	d := mustDoc(t, commented)
	if err := d.AddHost(Host{Name: "db01", Host: "10.0.0.10", Credentials: Credentials{PassEnv: "DB_PASS"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceHost("web01", Host{Name: "web01", Host: "10.0.0.100", Group: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveHost("web02"); err != nil {
		t.Fatal(err)
	}
	c, out := reparse(t, d)
	for _, want := range []string{"# top comment", "# default port", "# first host", "# trailing note"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost comment %q:\n%s", want, out)
		}
	}
	if len(c.Hosts) != 2 || c.Hosts[0].Host != "10.0.0.100" || c.Hosts[1].Name != "db01" || c.Hosts[1].PassEnv != "DB_PASS" {
		t.Errorf("hosts = %+v", c.Hosts)
	}
}

func TestDocumentErrors(t *testing.T) {
	d := mustDoc(t, commented)
	if err := d.AddHost(Host{Name: "web01", Host: "x"}); !errors.Is(err, ErrHostExists) {
		t.Errorf("add duplicate: %v", err)
	}
	if err := d.ReplaceHost("nope", Host{Name: "nope"}); !errors.Is(err, ErrHostNotFound) {
		t.Errorf("replace missing: %v", err)
	}
	if err := d.RemoveHost("nope"); !errors.Is(err, ErrHostNotFound) {
		t.Errorf("remove missing: %v", err)
	}
}

func TestDocumentEmptyAndFlowHosts(t *testing.T) {
	for _, raw := range []string{"", "defaults: {port: 22}\n", "hosts: []\n", "hosts:\n"} {
		d := mustDoc(t, raw)
		if err := d.AddHost(Host{Name: "a", Host: "h"}); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		c, out := reparse(t, d)
		if len(c.Hosts) != 1 || strings.Contains(out, "[") {
			t.Errorf("%q gave:\n%s", raw, out)
		}
	}
}

func TestMutateValidatesAndSaves(t *testing.T) {
	ctx := context.Background()
	s := &LocalStore{Path: filepath.Join(t.TempDir(), "hosts.yaml")}
	if err := s.Save(ctx, []byte(commented), ""); err != nil {
		t.Fatal(err)
	}

	// A change that breaks validation is rejected and nothing is written.
	err := Mutate(ctx, s, func(d *Document, _ *Config) error {
		return d.AddHost(Host{Name: "bad", Host: "h", Credentials: Credentials{Pass: "x", PassEnv: "X"}})
	})
	if err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Fatalf("err = %v", err)
	}
	if raw, _, _ := s.Load(ctx); string(raw) != commented {
		t.Error("invalid change was written")
	}

	if err := Mutate(ctx, s, func(d *Document, _ *Config) error {
		return d.AddHost(Host{Name: "ok", Host: "h"})
	}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s.Load(ctx)
	if !strings.Contains(string(raw), "name: ok") {
		t.Errorf("not saved:\n%s", raw)
	}
}

// conflictStore fails the first n saves with ErrVersionConflict.
type conflictStore struct {
	*LocalStore
	fail  int32
	saves atomic.Int32
}

func (c *conflictStore) Save(ctx context.Context, raw []byte, v string) error {
	if c.saves.Add(1) <= c.fail {
		return ErrVersionConflict
	}
	return c.LocalStore.Save(ctx, raw, v)
}

func TestMutateRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	local := &LocalStore{Path: filepath.Join(t.TempDir(), "hosts.yaml")}
	_ = local.Save(ctx, []byte(commented), "")

	s := &conflictStore{LocalStore: local, fail: 2}
	calls := 0
	err := Mutate(ctx, s, func(d *Document, _ *Config) error {
		calls++
		return d.RemoveHost("web02")
	})
	if err != nil || calls != 3 {
		t.Errorf("err=%v calls=%d", err, calls)
	}

	s = &conflictStore{LocalStore: local, fail: 99}
	err = Mutate(ctx, s, func(d *Document, _ *Config) error { return d.AddHost(Host{Name: "z", Host: "z"}) })
	if !errors.Is(err, ErrVersionConflict) {
		t.Errorf("persistent conflict err = %v", err)
	}
}

// Every Host and Defaults field must survive the encode path saves use
// (AddHost / SetDefaults / SetCommands, then a strict re-parse).
func TestDocumentRoundTripsEveryField(t *testing.T) {
	const arnSSM = "arn:aws:ssm:eu-west-1:123456789012:parameter/p"
	const arnSM = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:s#k"
	hosts := []Host{
		{Name: "a", Host: "10.0.0.1", Group: "g", Port: 2222, IdentityFile: "~/.ssh/a", KnownHosts: "~/.ssh/kh",
			Credentials: Credentials{User: "u", PassEnv: "A_PASS"}},
		{Name: "b", Host: "10.0.0.2", Credentials: Credentials{UserARN: arnSM, Pass: "p w"}},
		{Name: "c", Host: "10.0.0.3", Credential: "lab", Credentials: Credentials{UserEnv: "C_USER", PassARN: arnSSM}},
	}
	sets := map[string]CredentialSet{
		"lab": {Credentials: Credentials{User: "admin", PassEnv: "LAB_PASS"}, IdentityFile: "~/.ssh/lab"},
		"ops": {Credentials: Credentials{UserEnv: "OPS_USER", PassARN: arnSM}},
		"key": {Credentials: Credentials{UserARN: arnSSM, Pass: "x"}},
	}
	defaults := Defaults{
		Credential:  "ops",
		Credentials: Credentials{UserEnv: "D_USER", PassARN: arnSSM},
		Port:        22, Timeout: 9, IdentityFile: "~/.ssh/d", KnownHosts: "~/.ssh/dkh",
		HostKeyPolicy: HostKeyAcceptNew, Parallel: 4, Output: "json", Debug: true,
	}
	commands := []string{"uptime", "df -h"}

	d := mustDoc(t, "hosts: []\n")
	for _, h := range hosts {
		if err := d.AddHost(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetDefaults(defaults); err != nil {
		t.Fatal(err)
	}
	if err := d.SetCommands(commands); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lab", "ops", "key"} {
		if err := d.SetCredential(name, sets[name]); err != nil {
			t.Fatal(err)
		}
	}
	c, out := reparse(t, d)
	if err := c.Validate(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !reflect.DeepEqual(c.Hosts, hosts) || c.Defaults != defaults || !reflect.DeepEqual(c.Commands, commands) ||
		!reflect.DeepEqual(c.Credentials, sets) {
		t.Errorf("round trip changed the config:\n%s", out)
	}
	// A round trip can't catch a renamed tag (encode and decode change
	// together), so pin the documented key names per section.
	var raw struct {
		Defaults    map[string]any            `yaml:"defaults"`
		Credentials map[string]map[string]any `yaml:"credentials"`
		Hosts       []map[string]any          `yaml:"hosts"`
	}
	if err := yaml.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	hostKeys := map[string]bool{}
	for _, h := range raw.Hosts {
		for k := range h {
			hostKeys[k] = true
		}
	}
	wantKeys := func(section string, got map[string]bool, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s keys = %v, want %v", section, keys(got), want)
		}
		for _, k := range want {
			if !got[k] {
				t.Errorf("%s key %q missing (got %v)", section, k, keys(got))
			}
		}
	}
	wantKeys("host", hostKeys, "name", "host", "group", "port", "credential", "identity_file", "known_hosts",
		"user", "user_env", "user_arn", "pass", "pass_env", "pass_arn")
	setKeys := map[string]bool{}
	for _, set := range raw.Credentials {
		for k := range set {
			setKeys[k] = true
		}
	}
	wantKeys("credential set", setKeys, "user", "user_env", "user_arn", "pass", "pass_env", "pass_arn", "identity_file")
	defaultKeys := map[string]bool{}
	for k := range raw.Defaults {
		defaultKeys[k] = true
	}
	wantKeys("defaults", defaultKeys, "credential", "user_env", "pass_arn", "port", "timeout", "identity_file",
		"known_hosts", "host_key_policy", "parallel", "output", "debug")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCredentialSetDocument(t *testing.T) {
	d := mustDoc(t, "# top comment\ndefaults:\n  port: 22\nhosts:\n  - name: a\n    host: 10.0.0.1\n    credential: lab # uses lab\n")
	lab := CredentialSet{Credentials: Credentials{User: "admin", PassEnv: "LAB_PASS"}}
	if err := d.SetCredential("lab", lab); err != nil {
		t.Fatal(err)
	}
	c, out := reparse(t, d)
	if err := c.Validate(); err != nil || c.Credentials["lab"] != lab {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "# top comment") || !strings.Contains(out, "# uses lab") ||
		strings.Index(out, "credentials:") < strings.Index(out, "defaults:") || strings.Index(out, "credentials:") > strings.Index(out, "hosts:") {
		t.Errorf("comments or section order lost:\n%s", out)
	}

	lab.PassEnv = "OTHER"
	if err := d.SetCredential("lab", lab); err != nil {
		t.Fatal(err)
	}
	if c, _ = reparse(t, d); c.Credentials["lab"].PassEnv != "OTHER" || len(c.Credentials) != 1 {
		t.Errorf("replace: %+v", c.Credentials)
	}

	// Removing a set that a host uses leaves the reference for Validate.
	if err := d.RemoveCredential("lab"); err != nil {
		t.Fatal(err)
	}
	c, out = reparse(t, d)
	if strings.Contains(out, "credentials:") {
		t.Errorf("empty section kept:\n%s", out)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `host a: credential "lab" is not defined`) {
		t.Errorf("dangling reference: %v", err)
	}
	if err := d.RemoveCredential("lab"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("remove missing: %v", err)
	}
}

func TestSetDefaultCredential(t *testing.T) {
	for _, start := range []string{
		"credentials:\n  lab: {user: a}\nhosts: []\n",            // no defaults block
		"defaults:\ncredentials:\n  lab: {user: a}\nhosts: []\n", // empty (null) defaults
		"defaults:\n  port: 22 # ssh\ncredentials:\n  lab: {user: a}\nhosts: []\n",
	} {
		d := mustDoc(t, start)
		if err := d.SetDefaultCredential("lab"); err != nil {
			t.Fatal(err)
		}
		c, out := reparse(t, d)
		if c.Defaults.Credential != "lab" || c.Validate() != nil {
			t.Errorf("set from %q:\n%s", start, out)
		}
		if strings.Contains(start, "# ssh") && !strings.Contains(out, "port: 22 # ssh") {
			t.Errorf("comment lost:\n%s", out)
		}
		if err := d.SetDefaultCredential("lab2"); err != nil {
			t.Fatal(err)
		}
		if c, _ = reparse(t, d); c.Defaults.Credential != "lab2" {
			t.Errorf("replace: %q", c.Defaults.Credential)
		}
		if err := d.SetDefaultCredential(""); err != nil {
			t.Fatal(err)
		}
		if c, out = reparse(t, d); c.Defaults.Credential != "" || strings.Contains(out, "credential:") {
			t.Errorf("clear:\n%s", out)
		}
	}
}
