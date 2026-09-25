package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
	"github.com/mbevc1/mrsh/internal/ssh"
	"github.com/mbevc1/mrsh/internal/sshtest"
)

func testConfig() *config.Config {
	return &config.Config{
		Defaults: config.Defaults{Credentials: config.Credentials{User: "deploy", PassEnv: "DEF_PASS"}, Port: 2200, IdentityFile: "/keys/default"},
		Hosts: []config.Host{
			{Name: "web01", Host: "10.0.0.1", Group: "web"},
			{Name: "web02", Host: "10.0.0.2", Group: "web"},
			{Name: "db01", Host: "10.0.0.10", Group: "db"},
			{Name: "db01-alt", Host: "10.0.0.10", Group: "db", Port: 2222},
		},
	}
}

func names(ts []Target) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Name)
	}
	return strings.Join(s, ",")
}

func TestTargetsUnionDedupOrder(t *testing.T) {
	cfg := testConfig()
	ts, err := Targets(cfg, TargetOptions{
		Group: "web",
		Hosts: []string{"db01", "web01", "10.0.0.10", "1.2.3.4", "1.2.3.4", "admin@1.2.3.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The address 10.0.0.10 selects both entries sharing it; the repeated
	// literal collapses, but a different user makes a distinct target.
	if got := names(ts); got != "web01,web02,db01,db01-alt,1.2.3.4,admin@1.2.3.4" {
		t.Errorf("targets = %s", got)
	}
	if ts[3].Port != 2222 || ts[0].Port != 2200 {
		t.Errorf("ports: %d %d", ts[3].Port, ts[0].Port)
	}
	if ts[0].Literal || !ts[4].Literal {
		t.Error("literal flags wrong")
	}
}

func TestTargetsLogsLiteralFallthrough(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := Targets(testConfig(), TargetOptions{Hosts: []string{"web001"}}); err != nil {
		t.Fatal(err)
	}
	want := `--host \"web001\" not found in config; treating as literal address`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log missing %s:\n%s", want, buf.String())
	}
}

func TestLiteralParsing(t *testing.T) {
	tests := []struct {
		in       string
		opts     TargetOptions
		host     string
		port     int
		user     string
		identity string
		wantErr  bool
	}{
		{in: "10.0.0.5", host: "10.0.0.5", port: 2200, user: "deploy", identity: "/keys/default"},
		{in: "admin@10.0.0.5:2222", host: "10.0.0.5", port: 2222, user: "admin", identity: "/keys/default"},
		{in: "admin@10.0.0.5", opts: TargetOptions{User: "root", IdentityFile: "/keys/x"}, host: "10.0.0.5", port: 2200, user: "root", identity: "/keys/x"},
		{in: "[::1]:2222", host: "::1", port: 2222, user: "deploy", identity: "/keys/default"},
		{in: "::1", host: "::1", port: 2200, user: "deploy", identity: "/keys/default"},
		{in: "u@[fe80::1]", host: "fe80::1", port: 2200, user: "u", identity: "/keys/default"},
		{in: "host.example.com:22", host: "host.example.com", port: 22, user: "deploy", identity: "/keys/default"},
		{in: "h:0", wantErr: true},
		{in: "h:abc", wantErr: true},
		{in: "[::1", wantErr: true},
		{in: "[::1]x", wantErr: true},
		{in: "user@", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			tt.opts.Hosts = []string{tt.in}
			ts, err := Targets(testConfig(), tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("accepted %q: %+v", tt.in, ts)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := ts[0]
			if got.Host.Host != tt.host || got.Port != tt.port || got.User != tt.user || got.IdentityFile != tt.identity || !got.Literal {
				t.Errorf("got host=%q port=%d user=%q identity=%q literal=%v", got.Host.Host, got.Port, got.User, got.IdentityFile, got.Literal)
			}
			// Literals inherit the defaults' pass group.
			if got.PassEnv != "DEF_PASS" {
				t.Errorf("pass group not inherited: %+v", got.Credentials)
			}
		})
	}
}

type recordingResolver struct{ seen []string }

func (r *recordingResolver) Resolve(_ context.Context, hosts []config.Host) ([]config.Secrets, error) {
	out := make([]config.Secrets, len(hosts))
	for i, h := range hosts {
		r.seen = append(r.seen, h.Name)
		out[i] = config.Secrets{User: "u-" + h.Name, Pass: "p"}
	}
	return out, nil
}

func TestResolveOnlyTargets(t *testing.T) {
	ts, _ := Targets(testConfig(), TargetOptions{Hosts: []string{"web02"}})
	r := &recordingResolver{}
	if err := Resolve(context.Background(), r, ts); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.seen, ",") != "web02" || ts[0].Secrets.User != "u-web02" {
		t.Errorf("resolved %v, secrets %v", r.seen, ts[0].Secrets)
	}
}

func TestExecuteConcurrencyAndIsolation(t *testing.T) {
	var targets []Target
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		targets = append(targets, Target{Host: config.Host{Name: n, Host: n}})
	}
	var cur, peak atomic.Int32
	var mu sync.Mutex
	job := func(_ context.Context, tg Target) Result {
		n := cur.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		defer cur.Add(-1)
		time.Sleep(30 * time.Millisecond)
		switch tg.Name {
		case "b":
			return Result{ExitCode: -1, Err: errors.New("boom")}
		case "c":
			panic("kaboom")
		case "d":
			return Result{ExitCode: 7}
		}
		return Result{Stdout: "ok " + tg.Name}
	}

	results := Execute(context.Background(), targets, 2, job)
	if p := peak.Load(); p != 2 {
		t.Errorf("peak concurrency = %d, want 2", p)
	}
	for i, r := range results {
		if r.Name != targets[i].Name {
			t.Errorf("result %d is %s, want %s", i, r.Name, targets[i].Name)
		}
	}
	if results[0].Stdout != "ok a" || results[5].Stdout != "ok f" || results[0].Failed() {
		t.Errorf("healthy hosts affected: %+v %+v", results[0], results[5])
	}
	if results[1].Err == nil || results[2].Err == nil || !strings.Contains(results[2].Err.Error(), "kaboom") {
		t.Errorf("errors not isolated: %+v %+v", results[1], results[2])
	}
	if !results[3].Failed() || results[3].Err != nil {
		t.Errorf("non-zero exit: %+v", results[3])
	}
	if results[0].Duration < 30*time.Millisecond {
		t.Errorf("duration = %v", results[0].Duration)
	}
}

func TestRunCommandAgainstServer(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{User: "deploy", Password: "pw"})
	target := Target{
		Host:    config.Host{Name: "local", Host: srv.Host, Port: srv.Port},
		Secrets: config.Secrets{User: "deploy", Pass: "pw"},
	}
	job := RunCommand(CommandOptions{Command: "echo hi; exit 2", Timeout: 5 * time.Second, HostKeys: &ssh.HostKeyChecker{}, Policy: ssh.PolicyInsecure})
	r := job(context.Background(), target)
	if r.Err != nil || r.Stdout != "hi\n" || r.ExitCode != 2 {
		t.Errorf("result = %+v", r)
	}

	script := RunCommand(CommandOptions{Command: "sh -s", Stdin: []byte("echo script\n"), Timeout: 5 * time.Second, HostKeys: &ssh.HostKeyChecker{}, Policy: ssh.PolicyInsecure})
	if r := script(context.Background(), target); r.Err != nil || r.Stdout != "script\n" {
		t.Errorf("script result = %+v", r)
	}

	target.Secrets.Pass = "wrong"
	if r := job(context.Background(), target); r.Err == nil || r.ExitCode != -1 {
		t.Errorf("bad auth result = %+v", r)
	}
}

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	err := WriteText(&buf, []Result{
		{Name: "web01", Host: "10.0.0.1", Group: "web", Stdout: "up 4 days\nline2\nline3\n", Duration: 142 * time.Millisecond},
		{Name: "db01", Host: "10.0.0.10", ExitCode: 1, Stderr: "permission denied\n", Duration: 1500 * time.Millisecond},
		{Name: "x", Host: "x", ExitCode: -1, Err: errors.New("dial x:22: refused")},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"NAME    HOST        GROUP   EXIT   DURATION   OUTPUT",
		"up 4 days (+2 lines)", "142ms", "1.5s",
		"[stderr] permission denied", "[error] dial x:22: refused",
		"db01    10.0.0.10   -       1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWarnIfInsecure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	WarnIfInsecure(ssh.PolicyStrict, 3)
	if buf.Len() != 0 {
		t.Errorf("warned for strict: %s", buf.String())
	}
	WarnIfInsecure(ssh.PolicyInsecure, 3)
	if !strings.Contains(buf.String(), "host key checking disabled") {
		t.Errorf("no warning: %q", buf.String())
	}
}
