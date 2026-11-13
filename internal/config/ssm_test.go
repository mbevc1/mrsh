package config

import (
	"context"
	"errors"
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
