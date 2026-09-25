// Package sshtest runs an in-process SSH server for tests. Commands execute
// through the local "sh -c", with real stdin, stdout, stderr and exit codes.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	gliderssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Options selects the auth methods and host keys the server offers.
type Options struct {
	User                string
	Password            string // enables password auth
	KeyboardInteractive bool   // answer with Password via keyboard-interactive instead
	AuthorizedKey       gossh.PublicKey
	HostSigners         []gossh.Signer // default: one fresh ed25519 key
}

// Server is a running test server.
type Server struct {
	Host     string
	Port     int
	HostKeys []gossh.PublicKey
}

// Addr returns host:port.
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Start runs a server until the test ends.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	if len(opts.HostSigners) == 0 {
		opts.HostSigners = []gossh.Signer{NewEd25519Signer(t)}
	}
	userOK := func(ctx gliderssh.Context) bool { return opts.User == "" || ctx.User() == opts.User }

	srv := &gliderssh.Server{Handler: handle}
	for _, s := range opts.HostSigners {
		srv.AddHostKey(s)
	}
	if opts.Password != "" && !opts.KeyboardInteractive {
		srv.PasswordHandler = func(ctx gliderssh.Context, pass string) bool {
			return userOK(ctx) && pass == opts.Password
		}
	}
	if opts.KeyboardInteractive {
		srv.KeyboardInteractiveHandler = func(ctx gliderssh.Context, ch gossh.KeyboardInteractiveChallenge) bool {
			answers, err := ch("", "", []string{"Password: "}, []bool{false})
			return err == nil && userOK(ctx) && len(answers) == 1 && answers[0] == opts.Password
		}
	}
	if opts.AuthorizedKey != nil {
		srv.PublicKeyHandler = func(ctx gliderssh.Context, key gliderssh.PublicKey) bool {
			return userOK(ctx) && gliderssh.KeysEqual(key, opts.AuthorizedKey)
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })

	s := &Server{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
	for _, sg := range opts.HostSigners {
		s.HostKeys = append(s.HostKeys, sg.PublicKey())
	}
	return s
}

func handle(s gliderssh.Session) {
	cmd := exec.CommandContext(s.Context(), "sh", "-c", s.RawCommand())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s.Stderr()
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		code = 255
	}
	_ = s.Exit(code)
}

// NewEd25519Signer returns a fresh ed25519 signer.
func NewEd25519Signer(t testing.TB) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// NewRSASigner returns a fresh 2048-bit RSA signer.
func NewRSASigner(t testing.TB) gossh.Signer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// WriteKey writes a fresh ed25519 private key (OpenSSH PEM) to a temp file,
// encrypted when passphrase is set. It returns the path and the public key.
func WriteKey(t testing.TB, passphrase string) (string, gossh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = gossh.MarshalPrivateKey(priv, "")
	} else {
		block, err = gossh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return path, sshPub
}
