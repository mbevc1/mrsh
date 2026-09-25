package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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
