package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeAWS struct {
	mu       sync.Mutex
	params   map[string]string
	secrets  map[string]string
	ssmCalls []string // "region:n"
	smCalls  []string
}

func (f *fakeAWS) GetParameters(_ context.Context, region string, arns []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ssmCalls = append(f.ssmCalls, fmt.Sprintf("%s:%d", region, len(arns)))
	if len(arns) > ssmBatchSize {
		return nil, fmt.Errorf("batch of %d exceeds limit", len(arns))
	}
	out := map[string]string{}
	for _, a := range arns {
		if v, ok := f.params[a]; ok {
			out[a] = v
		}
	}
	return out, nil
}

func (f *fakeAWS) GetSecretValue(_ context.Context, _ string, arn string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smCalls = append(f.smCalls, arn)
	v, ok := f.secrets[arn]
	if !ok {
		return "", errors.New("ResourceNotFoundException")
	}
	return v, nil
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveLiteralAndEnv(t *testing.T) {
	r := &Resolver{Getenv: env(map[string]string{"WEB_PASS": "from-env"})}
	got, err := r.Resolve(context.Background(), []Host{
		{Name: "a", Credentials: Credentials{User: "deploy", Pass: "env:NOT_A_REF"}},
		{Name: "b", Credentials: Credentials{UserEnv: "WEB_USER_MISSING", PassEnv: "WEB_PASS"}},
		{Name: "c"},
	})
	if got[0].User != "deploy" || got[0].Pass != "env:NOT_A_REF" {
		t.Errorf("literal not verbatim: %v", got[0])
	}
	if got[1].Pass != "from-env" {
		t.Errorf("env pass not resolved")
	}
	if got[2] != (Secrets{}) {
		t.Errorf("unset host resolved to %v", got[2])
	}
	if err == nil || !strings.Contains(err.Error(), "host b: user_env: environment variable WEB_USER_MISSING is unset or empty") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveARNDispatchAndField(t *testing.T) {
	fake := &fakeAWS{
		params:  map[string]string{ssmARN: "ssm-pass"},
		secrets: map[string]string{smARN: `{"user":"dbadmin","pass":"sm-pass","port":5432}`},
	}
	r := &Resolver{AWS: fake}
	got, err := r.Resolve(context.Background(), []Host{
		{Name: "db01", Credentials: Credentials{UserARN: smARN + "#user", PassARN: smARN + "#pass"}},
		{Name: "db02", Credentials: Credentials{PassARN: ssmARN}},
		{Name: "db03", Credentials: Credentials{UserARN: smARN + "#port"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != (Secrets{User: "dbadmin", Pass: "sm-pass"}) || got[1].Pass != "ssm-pass" || got[2].User != "5432" {
		t.Errorf("resolved = %+v", got)
	}
	if len(fake.smCalls) != 1 {
		t.Errorf("GetSecretValue calls = %d, want 1 (cached by ARN without #field)", len(fake.smCalls))
	}
	if len(fake.ssmCalls) != 1 {
		t.Errorf("GetParameters calls = %v", fake.ssmCalls)
	}
}

func TestResolveSSMBatching(t *testing.T) {
	fake := &fakeAWS{params: map[string]string{}}
	var hosts []Host
	for i := 0; i < 23; i++ {
		region := "eu-west-1"
		if i >= 20 {
			region = "us-east-1"
		}
		a := fmt.Sprintf("arn:aws:ssm:%s:123456789012:parameter/p%d", region, i)
		fake.params[a] = fmt.Sprintf("v%d", i)
		hosts = append(hosts, Host{Name: fmt.Sprintf("h%d", i), Credentials: Credentials{PassARN: a}})
	}
	// A duplicate reference must not be fetched twice.
	hosts = append(hosts, Host{Name: "dup", Credentials: Credentials{PassARN: hosts[0].PassARN}})

	got, err := (&Resolver{AWS: fake}).Resolve(context.Background(), hosts)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(fake.ssmCalls)
	if strings.Join(fake.ssmCalls, " ") != "eu-west-1:10 eu-west-1:10 us-east-1:3" {
		t.Errorf("calls = %v", fake.ssmCalls)
	}
	if got[22].Pass != "v22" || got[23].Pass != "v0" {
		t.Errorf("values misaligned: %v %v", got[22], got[23])
	}
}

func TestResolveErrors(t *testing.T) {
	fake := &fakeAWS{secrets: map[string]string{smARN: `{"user":"x"}`, smARN + "-plain": "not json"}}
	_, err := (&Resolver{AWS: fake}).Resolve(context.Background(), []Host{
		{Name: "a", Credentials: Credentials{PassARN: smARN + "#pass"}},
		{Name: "b", Credentials: Credentials{PassARN: smARN + "-plain#pass"}},
		{Name: "c", Credentials: Credentials{PassARN: ssmARN}},
		{Name: "d", Credentials: Credentials{PassARN: "arn:aws:secretsmanager:eu-west-1:123456789012:secret:gone"}},
	})
	for _, want := range []string{
		`host a: pass_arn ` + smARN + `#pass: secret has no JSON key "pass"`,
		"host b: pass_arn " + smARN + "-plain#pass: secret is not a JSON object",
		"host c: pass_arn " + ssmARN + ": parameter not found",
		"host d: pass_arn arn:aws:secretsmanager:eu-west-1:123456789012:secret:gone: ResourceNotFoundException",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "not json\"") {
		t.Errorf("error leaks secret body: %v", err)
	}
}

func TestResolveNoAWSBackend(t *testing.T) {
	_, err := (&Resolver{}).Resolve(context.Background(), []Host{{Name: "a", Credentials: Credentials{PassARN: ssmARN}}})
	if err == nil || !strings.Contains(err.Error(), "AWS backend") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveNeverLogsSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fake := &fakeAWS{
		params:  map[string]string{ssmARN: "ssm-s3cret"},
		secrets: map[string]string{smARN: `{"pass":"sm-s3cret"}`},
	}
	r := &Resolver{AWS: fake, Logger: log, Getenv: env(map[string]string{"E": "env-s3cret"})}
	got, err := r.Resolve(context.Background(), []Host{
		{Name: "a", Credentials: Credentials{Pass: "lit-s3cret"}},
		{Name: "b", Credentials: Credentials{PassEnv: "E"}},
		{Name: "c", Credentials: Credentials{PassARN: ssmARN}},
		{Name: "d", Credentials: Credentials{PassARN: smARN + "#pass"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("secrets value", "secrets", got[0])
	logs := buf.String() + fmt.Sprint(got) + fmt.Sprintf("%v %+v", got[1], got[2])
	if !strings.Contains(buf.String(), `via="pass_env=E"`) {
		t.Errorf("missing reference in debug log:\n%s", buf.String())
	}
	for _, secret := range []string{"lit-s3cret", "env-s3cret", "ssm-s3cret", "sm-s3cret"} {
		if strings.Contains(logs, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, logs)
		}
	}
}

func TestParseARN(t *testing.T) {
	a, err := parseARN(smARN + "#user")
	if err != nil || a.Service != "secretsmanager" || a.Region != "eu-west-1" || a.Base != smARN || a.Field != "user" {
		t.Errorf("parseARN = %+v, %v", a, err)
	}
	if a, err := parseARN("arn:aws-us-gov:ssm:us-gov-west-1:123456789012:parameter/x"); err != nil || a.Region != "us-gov-west-1" {
		t.Errorf("gov partition: %+v, %v", a, err)
	}
	for _, bad := range []string{"", "arn:aws:ssm:eu-west-1:123:parameter/x", smARN + "#", "arn:aws:secretsmanager:eu-west-1:123456789012:secret:"} {
		if _, err := parseARN(bad); err == nil {
			t.Errorf("parseARN(%q) accepted", bad)
		}
	}
}
