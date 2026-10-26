package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/sshtest"
	"github.com/mbevc1/mrsh/internal/webui"
)

// startUI runs `mrsh ui` on cfg and returns a function that POSTs to its API.
func startUI(t *testing.T, cfg string) func(path, body string) (int, []byte) {
	t.Helper()
	opened := make(chan string, 1)
	old := openBrowser
	openBrowser = func(u string) error { opened <- u; return nil }
	t.Cleanup(func() { openBrowser = old })

	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"-f", cfg, "ui", "--addr", "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	var url string
	select {
	case url = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("ui did not start")
	}
	m := regexp.MustCompile(`^http://([^/]+)/#token=([0-9a-f]+)$`).FindStringSubmatch(url)
	return func(path, body string) (int, []byte) {
		req, _ := http.NewRequest("POST", "http://"+m[1]+path, bytes.NewBufferString(body))
		req.Header.Set(webui.TokenHeader, m[2])
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
}

func testResults(t *testing.T, body []byte) map[string]webui.TestResult {
	t.Helper()
	var out struct{ Results []webui.TestResult }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	m := map[string]webui.TestResult{}
	for _, r := range out.Results {
		m[r.Name] = r
	}
	return m
}

func TestUIHostTest(t *testing.T) {
	_, routerCfg := startRouter(t) // rt1: fake RouterOS
	linux := sshtest.Start(t, sshtest.Options{User: "admin", Password: "pw",
		Exec: func(string) sshtest.Reply { return sshtest.Reply{Stdout: "sh: /system: not found\n", Code: 127} }})
	cfg := writeConfig(t, readFile(t, routerCfg)+fmt.Sprintf(
		"  - {name: box, host: %s, port: %d, pass: pw}\n"+
			"  - {name: down, host: 127.0.0.1, port: 1, pass: pw}\n"+
			"  - {name: noenv, host: 127.0.0.1, port: 1, pass_env: MRSH_TEST_UNSET_VAR}\n",
		linux.Host, linux.Port))
	post := startUI(t, cfg)

	code, body := post("/api/test", `{"hosts":[]}`)
	if code != 200 {
		t.Fatalf("test all: %d %s", code, body)
	}
	got := testResults(t, body)
	if len(got) != 4 {
		t.Fatalf("results = %v", got)
	}
	want := map[string]struct {
		ok     bool
		detail string
	}{
		"rt1":   {true, "RouterOS 7.14 · RB5009UG+S+ · firmware 7.13.2 → 7.14 pending"},
		"box":   {true, "connected (not RouterOS)"},
		"down":  {false, "connection refused"},
		"noenv": {false, "MRSH_TEST_UNSET_VAR"},
	}
	if s := got["down"].Summary; s != "connect: connection refused" {
		t.Errorf("down summary = %q", s)
	}
	if s := got["rt1"].Summary; s != got["rt1"].Detail {
		t.Errorf("ok summary %q != detail %q", s, got["rt1"].Detail)
	}
	for name, w := range want {
		r := got[name]
		if r.OK != w.ok || !bytes.Contains([]byte(r.Detail), []byte(w.detail)) {
			t.Errorf("%s = %+v, want ok=%v detail containing %q", name, r, w.ok, w.detail)
		}
	}

	if code, body := post("/api/test", `{"hosts":["rt1"]}`); code != 200 || len(testResults(t, body)) != 1 {
		t.Errorf("one host: %d %s", code, body)
	}
	// Only saved names: an address or unknown name is refused, never dialled.
	for _, h := range []string{"nope", "127.0.0.1"} {
		if code, body := post("/api/test", fmt.Sprintf(`{"hosts":[%q]}`, h)); code != 404 {
			t.Errorf("%s: %d %s", h, code, body)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShortError(t *testing.T) {
	opErr := func(errno syscall.Errno) error {
		return fmt.Errorf("dial 10.0.0.1:22: %w", &net.OpError{Op: "dial", Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: errno}})
	}
	tests := []struct {
		err  error
		want string
	}{
		{opErr(syscall.ECONNREFUSED), "connect: connection refused"},
		{opErr(syscall.EHOSTUNREACH), "connect: no route to host"},
		{fmt.Errorf("dial router.lan:22: %w", &net.DNSError{Err: "no such host", Name: "router.lan", IsNotFound: true}), "unknown host router.lan"},
		{fmt.Errorf("connect 10.0.0.1:22: %w", context.DeadlineExceeded), "timed out"},
		{errors.New("connect 10.0.0.1:22: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain"), "authentication failed"},
		{errors.New("host rt2: pass_env: environment variable MRSH_X is unset or empty"), "environment variable MRSH_X is unset or empty"},
		{errors.New("connect 10.0.0.1:22: ssh: handshake failed: host 10.0.0.1:22 is not in /k (add it with ssh-keyscan, or use --host-key-policy accept-new)"), "host 10.0.0.1:22 is not in /k"},
		{errors.New("plain"), "plain"},
	}
	for _, tt := range tests {
		if got := shortError(tt.err); got != tt.want {
			t.Errorf("shortError(%q) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
