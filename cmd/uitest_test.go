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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/mt"
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

func upgradeResults(t *testing.T, body []byte) map[string]webui.UpgradeResult {
	t.Helper()
	var out struct{ Results []webui.UpgradeResult }
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	m := map[string]webui.UpgradeResult{}
	for _, r := range out.Results {
		m[r.Name] = r
	}
	return m
}

func TestUIUpgradeOneStep(t *testing.T) {
	f, cfg := startRouter(t)
	post := startUI(t, cfg)
	available := "installed-version: 7.13.2\nlatest-version: 7.14\nstatus: New version is available\n"

	// Dry run reports the step and changes nothing.
	f.update = []string{available}
	code, body := post("/api/upgrade/dry", `{"hosts":["rt1"]}`)
	r := upgradeResults(t, body)["rt1"]
	if code != 200 || !r.OK || r.Action != "install" || r.Summary != "would install RouterOS 7.13.2 → 7.14" {
		t.Fatalf("dry: %d %+v", code, r)
	}
	if f.sentContains(mt.InstallUpdatesCmd) {
		t.Fatal("dry run installed")
	}

	// One click, no prior check: the device is planned and upgraded.
	code, body = post("/api/upgrade/apply", `{"hosts":["rt1"],"confirm":true}`)
	r = upgradeResults(t, body)["rt1"]
	if code != 200 || !r.OK || r.Action != "install" || !strings.Contains(r.Summary, "installing RouterOS 7.13.2 → 7.14") {
		t.Fatalf("apply: %d %+v", code, r)
	}
	if !f.sentContains(mt.InstallUpdatesCmd) {
		t.Errorf("install not sent: %v", f.sent())
	}

	// Next run: RouterOS current, firmware behind (rbSample) -> firmware step.
	f.update = []string{"installed-version: 7.14\nlatest-version: 7.14\nstatus: System is already up to date\n"}
	code, body = post("/api/upgrade/apply", `{"hosts":["rt1"],"confirm":true}`)
	if r = upgradeResults(t, body)["rt1"]; code != 200 || r.Action != "firmware" || !f.sentContains(mt.FirmwareUpgrade) || !f.sentContains(mt.RebootCmd) {
		t.Fatalf("firmware: %d %+v %v", code, r, f.sent())
	}

	// Up to date: the server re-plans, so apply changes nothing.
	f.rb = "current-firmware: 7.14\nupgrade-firmware: 7.14\n"
	before := len(f.sent())
	code, body = post("/api/upgrade/apply", `{"hosts":["rt1"],"confirm":true}`)
	if r = upgradeResults(t, body)["rt1"]; code != 200 || r.Action != "none" || r.Summary != "up to date (RouterOS 7.14)" {
		t.Fatalf("up to date: %d %+v", code, r)
	}
	for _, c := range f.sent()[before:] {
		if c == mt.InstallUpdatesCmd || c == mt.FirmwareUpgrade || c == mt.RebootCmd {
			t.Errorf("up-to-date device changed: %q", c)
		}
	}

	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/upgrade/apply", `{"hosts":["rt1"]}`, 400},                 // no confirm
		{"/api/upgrade/apply", `{"hosts":[],"confirm":true}`, 400},       // never "all"
		{"/api/upgrade/dry", `{"hosts":[]}`, 400},                        // never "all"
		{"/api/upgrade/apply", `{"hosts":["nope"],"confirm":true}`, 404}, // saved names only
		{"/api/upgrade/apply", `{"hosts":["127.0.0.1"],"confirm":true}`, 404},
	} {
		if code, body := post(tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, code, body)
		}
	}
}
