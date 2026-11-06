package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const smARN = "arn:aws:secretsmanager:eu-west-1:123456789012:secret:mrsh/db01"
const ssmARN = "arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/db02/pass"

func TestParseTestdata(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/hosts.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Hosts) != 4 || c.Defaults.Parallel != 5 || len(c.Commands) != 2 {
		t.Errorf("unexpected parse: %+v", c)
	}
	if c.Hosts[0].PassEnv != "WEB01_PASS" || c.Hosts[2].UserARN == "" {
		t.Errorf("credentials not decoded: %+v", c.Hosts)
	}
}

func TestParseAndValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring; "" means valid
	}{
		{"minimal", "hosts:\n  - {name: a, host: 10.0.0.1}\n", ""},
		{"empty file", "", ""},
		{"host named sops", "hosts:\n  - {name: sops, host: sops}\n", ""},
		{"port unset ok", "hosts:\n  - {name: a, host: h, port: 0}\n", ""},
		{"sops at byte 0", "sops:\n  version: 3.8.1\nhosts: []\n", "SOPS-encrypted"},
		{"sops after hosts", "hosts: []\nsops:\n  kms: []\n", "SOPS-encrypted"},
		{"react mts key", "mts:\n  - {name: a, host: h}\n", `rename it to "hosts"`},
		{"unknown key", "hosts:\n  - {name: a, host: h, pasword: x}\n", "pasword"},
		{"pass and pass_env", "hosts:\n  - {name: a, host: h, pass: x, pass_env: X}\n", "host a: pass: set only one"},
		{"user and user_arn", "hosts:\n  - {name: a, host: h, user: x, user_arn: '" + smARN + "'}\n", "host a: user: set only one"},
		{"defaults double set", "defaults: {pass: x, pass_arn: '" + ssmARN + "'}\nhosts: []\n", "defaults: pass: set only one"},
		{"duplicate name", "hosts:\n  - {name: a, host: h1}\n  - {name: a, host: h2}\n", "duplicate name"},
		{"missing name", "hosts:\n  - {host: h}\n", "host #1: name is required"},
		{"missing host", "hosts:\n  - {name: a}\n", "host a: host is required"},
		{"port too big", "hosts:\n  - {name: a, host: h, port: 70000}\n", "port 70000"},
		{"negative port", "hosts:\n  - {name: a, host: h, port: -1}\n", "port -1"},
		{"url host", "hosts:\n  - {name: a, host: 'ssh://h'}\n", "not a URL"},
		{"user in host", "hosts:\n  - {name: a, host: 'root@h'}\n", "must not include a user"},
		{"bad arn", "hosts:\n  - {name: a, host: h, pass_arn: 'arn:aws:s3:::bucket'}\n", "pass_arn"},
		{"ssm with field", "hosts:\n  - {name: a, host: h, pass_arn: '" + ssmARN + "#x'}\n", "#field is only supported"},
		{"ssm not parameter", "hosts:\n  - {name: a, host: h, pass_arn: 'arn:aws:ssm:eu-west-1:123456789012:document/x'}\n", "parameter/<name>"},
		{"bad output", "defaults: {output: yaml}\nhosts: []\n", "output"},
		{"bad host key policy", "defaults: {host_key_policy: yolo}\nhosts: []\n", "host_key_policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.yaml))
			if err == nil {
				err = c.Validate()
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSOPSError(t *testing.T) {
	if _, err := Parse([]byte("sops: {}\n")); !errors.Is(err, ErrSOPSEncrypted) {
		t.Fatalf("got %v, want ErrSOPSEncrypted", err)
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	c, err := Parse([]byte("hosts:\n  - {name: a, host: h, port: 70000}\n  - {name: a}\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Validate()
	for _, want := range []string{"port 70000", "duplicate name", "host is required"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v missing %q", err, want)
		}
	}
}

func TestEffective(t *testing.T) {
	home, _ := os.UserHomeDir()
	c := &Config{Defaults: Defaults{
		Credentials:  Credentials{User: "deploy", PassEnv: "DEFAULT_PASS"},
		Port:         2200,
		IdentityFile: "~/.ssh/id",
	}}

	e := c.Effective(Host{Name: "a", Host: "h"})
	if e.Port != 2200 || e.User != "deploy" || e.PassEnv != "DEFAULT_PASS" || e.IdentityFile != filepath.Join(home, ".ssh/id") {
		t.Errorf("defaults not applied: %+v", e)
	}

	// Any variant on the host replaces the defaults' whole field-group.
	e = c.Effective(Host{Name: "b", Host: "h", Port: 22, Credentials: Credentials{UserEnv: "B_USER", PassARN: ssmARN}})
	if e.Port != 22 || e.User != "" || e.UserEnv != "B_USER" || e.PassEnv != "" || e.PassARN != ssmARN {
		t.Errorf("field-group override wrong: %+v", e)
	}

	if e := (&Config{}).Effective(Host{Name: "c", Host: "h"}); e.Port != DefaultPort {
		t.Errorf("port = %d, want %d", e.Port, DefaultPort)
	}
}

func TestSelect(t *testing.T) {
	c := &Config{Hosts: []Host{
		{Name: "web01", Host: "10.0.0.1", Group: "web"},
		{Name: "web02", Host: "10.0.0.2", Group: "web"},
		{Name: "db01", Host: "10.0.0.10", Group: "db"},
	}}
	names := func(hs []Host) string {
		var s []string
		for _, h := range hs {
			s = append(s, h.Name)
		}
		return strings.Join(s, ",")
	}

	if got, _ := c.Select("", nil); names(got) != "web01,web02,db01" {
		t.Errorf("no filter = %s", names(got))
	}
	if got, _ := c.Select("web", nil); names(got) != "web01,web02" {
		t.Errorf("group = %s", names(got))
	}
	got, unmatched := c.Select("web", []string{"10.0.0.10", "web01", "web001"})
	if names(got) != "web01,web02,db01" {
		t.Errorf("union = %s", names(got))
	}
	if strings.Join(unmatched, ",") != "web001" {
		t.Errorf("unmatched = %v", unmatched)
	}
}

func TestSecretRefDisplay(t *testing.T) {
	tests := []struct {
		ref      SecretRef
		kind     string
		masked   string
		unmasked string
	}{
		{SecretRef{Literal: "hunter2"}, KindPlain, Mask, "hunter2"},
		{SecretRef{Env: "X"}, KindEnv, "env:X", "env:X"},
		{SecretRef{ARN: ssmARN}, KindARN, ssmARN, ssmARN},
		{SecretRef{}, KindUnset, "", ""},
	}
	for _, tt := range tests {
		if got := tt.ref.Kind(); got != tt.kind {
			t.Errorf("Kind(%+v) = %s, want %s", tt.ref, got, tt.kind)
		}
		if got := tt.ref.Display(false); got != tt.masked {
			t.Errorf("Display(false) = %q, want %q", got, tt.masked)
		}
		if got := tt.ref.Display(true); got != tt.unmasked {
			t.Errorf("Display(true) = %q, want %q", got, tt.unmasked)
		}
	}
}

func TestEffectiveCredentialPrecedence(t *testing.T) {
	c := &Config{
		Credentials: map[string]CredentialSet{
			"lab":  {Credentials: Credentials{User: "lab-user", PassEnv: "LAB_PASS"}, IdentityFile: "/k/lab"},
			"base": {Credentials: Credentials{UserEnv: "BASE_USER", PassARN: "arn:base"}, IdentityFile: "/k/base"},
		},
		Defaults: Defaults{Credential: "base", Credentials: Credentials{User: "def-user"}},
	}
	tests := []struct {
		name       string
		h          Host
		user, pass string
		key        string
	}{
		// Nothing on the host: defaults' own user, then the defaults' set for the rest.
		{"defaults then defaults' set", Host{}, "def-user", "arn:base", "/k/base"},
		// The host's set beats defaults for every field it sets.
		{"host set", Host{Credential: "lab"}, "lab-user", "LAB_PASS", "/k/lab"},
		// The host's own fields beat its set, field by field.
		{"host overrides user only", Host{Credential: "lab", Credentials: Credentials{UserEnv: "MINE"}}, "MINE", "LAB_PASS", "/k/lab"},
		{"host overrides pass only", Host{Credential: "lab", Credentials: Credentials{Pass: "own"}}, "lab-user", "own", "/k/lab"},
		{"host overrides key only", Host{Credential: "lab", IdentityFile: "/k/own"}, "lab-user", "LAB_PASS", "/k/own"},
	}
	for _, tt := range tests {
		e := c.Effective(tt.h)
		user := e.User + e.UserEnv + e.UserARN
		pass := e.Pass + e.PassEnv + e.PassARN
		if user != tt.user || pass != tt.pass || e.IdentityFile != tt.key {
			t.Errorf("%s: user=%q pass=%q key=%q, want %q %q %q", tt.name, user, pass, e.IdentityFile, tt.user, tt.pass, tt.key)
		}
		if n := e.UserRef().variants() + e.PassRef().variants(); n != 2 {
			t.Errorf("%s: a field-group mixed variants from two places: %+v", tt.name, e.Credentials)
		}
	}
}

func TestValidateCredentialSets(t *testing.T) {
	const arn = "arn:aws:ssm:eu-west-1:123456789012:parameter/p"
	tests := []struct {
		name, yaml, want string
	}{
		{"ok", "credentials:\n  lab: {user: admin, pass_arn: " + arn + "}\nhosts:\n  - {name: a, host: h, credential: lab}\n", ""},
		{"unknown on host", "hosts:\n  - {name: a, host: h, credential: nope}\n", `host a: credential "nope" is not defined`},
		{"unknown on defaults", "defaults: {credential: nope}\nhosts: []\n", `defaults: credential "nope" is not defined`},
		{"empty set", "credentials:\n  lab: {}\nhosts: []\n", "credential lab: set at least one of user, pass or identity_file"},
		{"two pass variants", "credentials:\n  lab: {pass: x, pass_env: Y}\nhosts: []\n", "credential lab: pass: set only one of"},
		{"bad arn", "credentials:\n  lab: {pass_arn: nope}\nhosts: []\n", "credential lab: pass_arn:"},
		{"bad name", "credentials:\n  \"my lab\": {user: a}\nhosts: []\n", `credential "my lab": name must not contain whitespace`},
		{"unknown key", "credentials:\n  lab: {usr: a}\nhosts: []\n", "field usr not found"},
	}
	for _, tt := range tests {
		c, err := Parse([]byte(tt.yaml))
		if err == nil {
			err = c.Validate()
		}
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}

	c, _ := Parse([]byte("defaults: {credential: lab}\ncredentials:\n  lab: {user: a}\nhosts:\n  - {name: a, host: h, credential: lab}\n  - {name: b, host: h}\n"))
	if got := c.CredentialUsers("lab"); !reflect.DeepEqual(got, []string{"defaults", "a"}) {
		t.Errorf("CredentialUsers = %v", got)
	}
}
