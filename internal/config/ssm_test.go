package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mbevc1/mrsh/internal/ssmtest"
)

func TestSSMNames(t *testing.T) {
	c := &Config{}
	for _, tc := range []struct{ owner, name, field, want string }{
		{OwnerCredential, "routers", "pass", "/mrsh/creds/routers/pass"},
		{OwnerHost, "rb02", "user", "/mrsh/hosts/rb02/user"},
		{OwnerDefaults, "", "pass", "/mrsh/defaults/pass"},
	} {
		if got := c.SSMName(tc.owner, tc.name, tc.field); got != tc.want {
			t.Errorf("SSMName(%s, %s, %s) = %q, want %q", tc.owner, tc.name, tc.field, got, tc.want)
		}
	}
	c.Defaults.SSMPrefix = "/team-a/mrsh"
	if got := c.SSMName(OwnerHost, "rb02", "pass"); got != "/team-a/mrsh/hosts/rb02/pass" {
		t.Errorf("with prefix: %q", got)
	}

	for arn, want := range map[string]bool{
		"arn:aws:ssm:eu-west-1:123456789012:parameter/team-a/mrsh/hosts/rb02/pass": true,
		"arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/hosts/rb02/pass":        false, // another prefix
		"arn:aws:ssm:eu-west-1:123456789012:parameter/team-a/mrshx/pass":           false,
		"arn:aws:secretsmanager:eu-west-1:123456789012:secret:team-a/mrsh/x":       false,
	} {
		if _, ok := c.ManagedSSMName(arn); ok != want {
			t.Errorf("ManagedSSMName(%s) = %v, want %v", arn, ok, want)
		}
	}
}

func TestValidSSMName(t *testing.T) {
	for name, ok := range map[string]bool{
		"/mrsh/creds/routers/pass": true, "/a": true, "/a.b-c_d/e": true,
		"mrsh/x": false, "/mrsh/": false, "/mrsh//x": false, "/has space": false,
		"/aws/x": false, "/SSM-things/x": false, "/awsome/x": false,
	} {
		if err := ValidSSMName(name); (err == nil) != ok {
			t.Errorf("ValidSSMName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
	c := &Config{Defaults: Defaults{SSMPrefix: "/mrsh/"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ssm_prefix") {
		t.Errorf("trailing slash prefix: %v", err)
	}
}

func TestSecretWriterRoundTrip(t *testing.T) {
	fake := ssmtest.Start(t)
	w := NewSecretWriter()
	ctx := context.Background()

	arn, err := w.PutSecret(ctx, "/mrsh/creds/routers/pass", "s3cret", false)
	if err != nil || arn != fake.ARN("/mrsh/creds/routers/pass") {
		t.Fatalf("put: %q, %v", arn, err)
	}
	p := fake.Param("/mrsh/creds/routers/pass")
	if p == nil || p.Value != "s3cret" || p.Type != "SecureString" || p.Tags["managed-by"] != "mrsh" {
		t.Fatalf("stored = %+v", p)
	}

	// Never over an existing value unless asked.
	if _, err := w.PutSecret(ctx, "/mrsh/creds/routers/pass", "other", false); !errors.Is(err, ErrSecretExists) {
		t.Errorf("second put: %v", err)
	}
	if _, err := w.PutSecret(ctx, "/mrsh/creds/routers/pass", "rotated", true); err != nil || fake.Param("/mrsh/creds/routers/pass").Value != "rotated" {
		t.Errorf("overwrite: %v", err)
	}

	// The resolver reads it back through the same ARN.
	got, err := NewResolver().Resolve(ctx, []Host{{Name: "rt1", Credentials: Credentials{PassARN: arn}}})
	if err != nil || got[0].Pass != "rotated" {
		t.Errorf("resolve: %v", err)
	}

	if err := w.DeleteSecret(ctx, arn); err != nil || fake.Param("/mrsh/creds/routers/pass") != nil {
		t.Errorf("delete: %v", err)
	}
	if err := w.DeleteSecret(ctx, arn); err != nil {
		t.Errorf("deleting a missing parameter should be a no-op: %v", err)
	}
	if _, err := w.PutSecret(ctx, "no-leading-slash", "x", false); err == nil {
		t.Error("accepted an unqualified name")
	}
}

func TestPassSource(t *testing.T) {
	c := &Config{
		Defaults:    Defaults{Credential: "base"},
		Credentials: map[string]CredentialSet{"base": {Credentials: Credentials{Pass: "b"}}, "lab": {Credentials: Credentials{PassEnv: "L"}}, "keys": {IdentityFile: "~/.ssh/k"}},
	}
	for h, want := range map[Host]string{
		{Name: "own", Credentials: Credentials{PassARN: "arn:aws:ssm:eu-west-1:123456789012:parameter/x"}}: "host own",
		{Name: "set", Credential: "lab"}:   "credential lab",
		{Name: "keys", Credential: "keys"}: "credential base", // its set has no pass: falls through to defaults' set
		{Name: "plain"}:                    "credential base",
	} {
		if got := c.PassSource(h); got != want {
			t.Errorf("PassSource(%s) = %q, want %q", h.Name, got, want)
		}
	}
	c.Defaults.PassEnv = "D"
	if got := c.PassSource(Host{Name: "plain"}); got != "defaults" {
		t.Errorf("defaults own pass: %q", got)
	}
	if n, ok := SSMParamName("arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/hosts/a/pass"); !ok || n != "/mrsh/hosts/a/pass" {
		t.Errorf("SSMParamName = %q, %v", n, ok)
	}
}

func TestHostSecretDeletes(t *testing.T) {
	arn := func(n string) string { return "arn:aws:ssm:eu-west-1:123456789012:parameter" + n }
	c := &Config{
		Credentials: map[string]CredentialSet{"lab": {Credentials: Credentials{PassARN: arn("/mrsh/creds/lab/pass")}}},
		Hosts: []Host{
			{Name: "a", Credentials: Credentials{PassARN: arn("/mrsh/shared")}},
			{Name: "b", Credentials: Credentials{PassARN: arn("/mrsh/shared"), UserARN: arn("/mrsh/hosts/b/user")}},
			{Name: "c", Credentials: Credentials{PassARN: arn("/mrsh/creds/lab/pass")}},
			{Name: "d", Credentials: Credentials{PassARN: arn("/elsewhere/d")}},
		},
	}
	// Removed together, a and b take their shared parameter with them.
	del, kept := c.HostSecretDeletes([]string{"a", "b", "c", "d"})
	if fmt.Sprint(del) != "["+arn("/mrsh/shared")+" "+arn("/mrsh/hosts/b/user")+"]" {
		t.Errorf("del = %v", del)
	}
	if len(kept) != 2 || !strings.Contains(kept[0], "still used by credential lab") || !strings.Contains(kept[1], "not under /mrsh") {
		t.Errorf("kept = %v", kept)
	}
	// Alone, a leaves it for b.
	if del, kept := c.HostSecretDeletes([]string{"a"}); len(del) != 0 || !strings.Contains(fmt.Sprint(kept), "still used by host b") {
		t.Errorf("a alone: %v %v", del, kept)
	}
}
