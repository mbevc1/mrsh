package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/mbevc1/mrsh/internal/sshtest"
)

func baseConfig(s *sshtest.Server) Config {
	return Config{
		Host: s.Host, Port: s.Port, User: "u", Timeout: 5 * time.Second,
		HostKeys: insecureVerifier{}, AgentSocket: "-",
	}
}

func dialRun(t *testing.T, cfg Config, cmd string) (string, string, int, error) {
	t.Helper()
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		return "", "", -1, err
	}
	defer func() { _ = c.Close() }()
	return c.Run(context.Background(), cmd, nil)
}

func TestPasswordAuthAndOutput(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{User: "u", Password: "pw"})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"

	stdout, stderr, code, err := dialRun(t, cfg, "echo out; echo err >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "out\n" || stderr != "err\n" || code != 3 {
		t.Errorf("got stdout=%q stderr=%q code=%d", stdout, stderr, code)
	}
}

func TestWrongPassword(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{User: "u", Password: "pw"})
	cfg := baseConfig(srv)
	cfg.Pass = "nope"
	if _, _, _, err := dialRun(t, cfg, "true"); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("err = %v", err)
	}
}

func TestKeyboardInteractive(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{User: "u", Password: "pw", KeyboardInteractive: true})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"
	if out, _, _, err := dialRun(t, cfg, "echo ok"); err != nil || out != "ok\n" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestPublicKeyAuth(t *testing.T) {
	keyPath, pub := sshtest.WriteKey(t, "")
	srv := sshtest.Start(t, sshtest.Options{User: "u", AuthorizedKey: pub})
	cfg := baseConfig(srv)
	cfg.KeyFile = keyPath
	if out, _, _, err := dialRun(t, cfg, "echo key"); err != nil || out != "key\n" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestEncryptedKeyRejected(t *testing.T) {
	keyPath, _ := sshtest.WriteKey(t, "secret")
	cfg := Config{Host: "127.0.0.1", Port: 1, KeyFile: keyPath, HostKeys: insecureVerifier{}, AgentSocket: "-"}
	_, err := Dial(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "passphrase-protected") {
		t.Errorf("err = %v", err)
	}
}

func TestAgentAuth(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv := sshtest.Start(t, sshtest.Options{User: "u", AuthorizedKey: signer.PublicKey()})

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortTempDir(t), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, c) }()
		}
	}()

	cfg := baseConfig(srv)
	cfg.AgentSocket = sock
	if out, _, _, err := dialRun(t, cfg, "echo agent"); err != nil || out != "agent\n" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestNoAuthMethod(t *testing.T) {
	cfg := Config{Host: "127.0.0.1", Port: 1, HostKeys: insecureVerifier{}, AgentSocket: "-"}
	if _, err := Dial(context.Background(), cfg); !errors.Is(err, ErrNoAuthMethod) {
		t.Errorf("err = %v", err)
	}
}

func TestStdin(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Password: "pw"})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	out, _, code, err := c.Run(context.Background(), "sh -s", strings.NewReader("echo from-script\nexit 4\n"))
	if err != nil || out != "from-script\n" || code != 4 {
		t.Errorf("out=%q code=%d err=%v", out, code, err)
	}
}

func TestContextCancelKillsCommand(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Password: "pw"})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, code, err := c.Run(ctx, "sleep 30", nil)
	if !errors.Is(err, context.DeadlineExceeded) || code != -1 {
		t.Errorf("code=%d err=%v", code, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	// A listener that accepts but never speaks SSH.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	cfg := Config{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, Pass: "x",
		Timeout: 300 * time.Millisecond, HostKeys: insecureVerifier{}, AgentSocket: "-"}
	start := time.Now()
	if _, err := Dial(context.Background(), cfg); err == nil {
		t.Fatal("expected timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %v", d)
	}
}

func TestHostKeyPolicies(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Password: "pw"})
	known := filepath.Join(t.TempDir(), "sub", "known_hosts")
	checker := &HostKeyChecker{}
	cfgWith := func(policy string) Config {
		v, err := checker.ForPolicy(policy, known)
		if err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig(srv)
		cfg.Pass, cfg.HostKeys = "pw", v
		return cfg
	}

	// strict: a missing file is an error.
	if _, _, _, err := dialRun(t, cfgWith(PolicyStrict), "true"); err == nil {
		t.Error("strict with missing known_hosts connected")
	}
	// accept-new: creates the file and records the key.
	if _, _, _, err := dialRun(t, cfgWith(PolicyAcceptNew), "true"); err != nil {
		t.Fatalf("accept-new: %v", err)
	}
	raw, _ := os.ReadFile(known)
	if !strings.Contains(string(raw), knownhosts.Normalize(srv.Addr())) {
		t.Fatalf("known_hosts not written:\n%s", raw)
	}
	if fi, _ := os.Stat(known); fi.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %v", fi.Mode().Perm())
	}
	// strict now succeeds.
	if _, _, _, err := dialRun(t, cfgWith(PolicyStrict), "true"); err != nil {
		t.Fatalf("strict after accept-new: %v", err)
	}

	// A different key on the same address is always rejected.
	other := sshtest.Start(t, sshtest.Options{Password: "pw"})
	line := knownhosts.Line([]string{knownhosts.Normalize(other.Addr())}, srv.HostKeys[0])
	if err := os.WriteFile(known, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{PolicyStrict, PolicyAcceptNew} {
		v, _ := checker.ForPolicy(policy, known)
		cfg := baseConfig(other)
		cfg.Pass, cfg.HostKeys = "pw", v
		if _, _, _, err := dialRun(t, cfg, "true"); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Errorf("%s with changed key: err = %v", policy, err)
		}
	}
}

func TestStrictUnknownHostHint(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Password: "pw"})
	known := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(known, nil, 0o600)
	v, _ := (&HostKeyChecker{}).ForPolicy(PolicyStrict, known)
	cfg := baseConfig(srv)
	cfg.Pass, cfg.HostKeys = "pw", v
	if _, _, _, err := dialRun(t, cfg, "true"); err == nil || !strings.Contains(err.Error(), "ssh-keyscan") {
		t.Errorf("err = %v", err)
	}
}

// A host known only by its ed25519 key must connect even when the server
// also offers (and would by default prefer) an RSA key.
func TestKnownAlgorithmsSelection(t *testing.T) {
	ed := sshtest.NewEd25519Signer(t)
	srv := sshtest.Start(t, sshtest.Options{Password: "pw", HostSigners: []gossh.Signer{sshtest.NewRSASigner(t), ed}})
	known := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(srv.Addr())}, ed.PublicKey())
	_ = os.WriteFile(known, []byte(line+"\n"), 0o600)

	v, _ := (&HostKeyChecker{}).ForPolicy(PolicyStrict, known)
	cfg := baseConfig(srv)
	cfg.Pass, cfg.HostKeys = "pw", v
	if _, _, _, err := dialRun(t, cfg, "true"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestForPolicyErrors(t *testing.T) {
	h := &HostKeyChecker{}
	if _, err := h.ForPolicy("yolo", "x"); err == nil {
		t.Error("unknown policy accepted")
	}
	if _, err := h.ForPolicy(PolicyStrict, ""); err == nil {
		t.Error("strict without path accepted")
	}
}

// shortTempDir keeps unix socket paths under the 104-byte limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "mrsh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestDownloadAndStat(t *testing.T) {
	root := t.TempDir()
	payload := strings.Repeat("0123456789", 100_000) // 1 MB
	if err := os.WriteFile(filepath.Join(root, "backup-Mon.backup"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := sshtest.Start(t, sshtest.Options{Password: "pw", SFTPRoot: root})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"
	c, err := Dial(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	size, err := c.Stat("backup-Mon.backup")
	if err != nil || size != int64(len(payload)) {
		t.Fatalf("Stat = %d, %v", size, err)
	}
	var buf strings.Builder
	n, err := c.Download(context.Background(), "backup-Mon.backup", &buf)
	if err != nil || n != int64(len(payload)) || buf.String() != payload {
		t.Fatalf("Download n=%d err=%v match=%v", n, err, buf.String() == payload)
	}
	if _, err := c.Download(context.Background(), "missing.rsc", &buf); err == nil {
		t.Error("missing file downloaded")
	}
	// Run still works on the same connection after SFTP use.
	if out, _, _, err := c.Run(context.Background(), "echo still", nil); err != nil || out != "still\n" {
		t.Errorf("run after sftp: %q %v", out, err)
	}
}

func TestDroppedSessionIsDisconnected(t *testing.T) {
	srv := sshtest.Start(t, sshtest.Options{Password: "pw", Exec: func(string) sshtest.Reply {
		return sshtest.Reply{Stdout: "Rebooting...\n", Drop: true}
	}})
	cfg := baseConfig(srv)
	cfg.Pass = "pw"
	_, _, code, err := dialRun(t, cfg, "/system reboot")
	if !errors.Is(err, ErrDisconnected) || code != -1 {
		t.Errorf("code=%d err=%v", code, err)
	}
}
