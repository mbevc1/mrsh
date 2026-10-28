// Package ssh provides the SSH client used to run commands and download files.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ErrDisconnected means the session or connection ended without an exit
// status, as when the remote side reboots.
var ErrDisconnected = errors.New("session closed without exit status")

// ErrNoAuthMethod means no key, agent or password is available for a host.
var ErrNoAuthMethod = errors.New("no SSH auth method available (set identity_file, run ssh-agent, or configure pass)")

// Config describes one connection. The zero value of each field means unset.
type Config struct {
	Host    string
	Port    int
	User    string
	Pass    string
	KeyFile string
	Timeout time.Duration // dial + handshake
	// HostKeys verifies the server key. Required: use HostKeyChecker.
	HostKeys HostKeyVerifier
	// AgentSocket overrides $SSH_AUTH_SOCK; "-" disables the agent.
	AgentSocket string
}

// HostKeyVerifier supplies the host-key callback and the preferred host-key
// algorithms for an address.
type HostKeyVerifier interface {
	Verify(addr string) (gossh.HostKeyCallback, []string, error)
}

// Conn is one established SSH connection. It is safe for sequential use by
// one goroutine; open one Conn per host.
type Conn struct {
	client *gossh.Client
	addr   string
	agent  net.Conn
	sftp   *sftp.Client // opened on first file operation
}

// Addr returns host:port for c.
func (cfg Config) Addr() string {
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(port))
}

// Dial connects and authenticates. Auth order: key file, then SSH agent,
// then password (plus keyboard-interactive answered with the password).
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.HostKeys == nil {
		return nil, errors.New("ssh: no host key verifier configured")
	}
	addr := cfg.Addr()
	auth, names, agentConn, err := authMethods(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	closeAgent := func() {
		if agentConn != nil {
			_ = agentConn.Close()
		}
	}

	callback, algos, err := cfg.HostKeys.Verify(addr)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("%s: %w", addr, err)
	}
	clientCfg := &gossh.ClientConfig{
		User:              cfg.User,
		Auth:              auth,
		HostKeyCallback:   callback,
		HostKeyAlgorithms: algos,
		Timeout:           cfg.Timeout,
	}

	slog.Debug("ssh dial", "addr", addr, "user", cfg.User, "auth", strings.Join(names, ","))
	start := time.Now()
	d := net.Dialer{Timeout: cfg.Timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	// Bound the handshake too; the SSH library only applies Timeout to its own dial.
	if cfg.Timeout > 0 {
		_ = nc.SetDeadline(time.Now().Add(cfg.Timeout))
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	sc, chans, reqs, err := gossh.NewClientConn(nc, addr, clientCfg)
	stop()
	if err != nil {
		_ = nc.Close()
		closeAgent()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("connect %s: %w", addr, ctx.Err())
		}
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	_ = nc.SetDeadline(time.Time{})
	slog.Debug("ssh connected", "addr", addr, "handshake", time.Since(start))
	return &Conn{client: gossh.NewClient(sc, chans, reqs), addr: addr, agent: agentConn}, nil
}

// authMethods builds the auth chain and names each method for debug logs.
// Secrets never appear in the names.
func authMethods(cfg Config) ([]gossh.AuthMethod, []string, net.Conn, error) {
	var methods []gossh.AuthMethod
	var names []string

	if cfg.KeyFile != "" {
		signer, err := loadKey(cfg.KeyFile)
		if err != nil {
			return nil, nil, nil, err
		}
		methods = append(methods, gossh.PublicKeys(signer))
		names = append(names, "publickey")
	}

	var agentConn net.Conn
	sock := cfg.AgentSocket
	if sock == "" {
		sock = os.Getenv("SSH_AUTH_SOCK")
	}
	if sock != "" && sock != "-" && runtime.GOOS != "windows" {
		if c, err := net.Dial("unix", sock); err == nil {
			agentConn = c
			methods = append(methods, gossh.PublicKeysCallback(agent.NewClient(c).Signers))
			names = append(names, "agent")
		} else {
			slog.Debug("ssh agent unavailable", "err", err)
		}
	}

	if cfg.Pass != "" {
		pass := cfg.Pass
		methods = append(methods,
			gossh.Password(pass),
			gossh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pass
				}
				return answers, nil
			}),
		)
		names = append(names, "password", "keyboard-interactive")
	}

	if len(methods) == 0 {
		return nil, nil, nil, ErrNoAuthMethod
	}
	return methods, names, agentConn, nil
}

func loadKey(path string) (gossh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity file: %w", err)
	}
	signer, err := gossh.ParsePrivateKey(pem)
	var missing *gossh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, fmt.Errorf("identity file %s is passphrase-protected; encrypted keys are not supported, load it into ssh-agent instead", path)
	}
	if err != nil {
		return nil, fmt.Errorf("parse identity file %s: %w", path, err)
	}
	return signer, nil
}

// Run executes cmd in a new session, feeding stdin when non-nil. A non-zero
// remote exit status is reported through exitCode with a nil error; err is
// set only when the command could not run or finish (exitCode is then -1).
// Cancelling ctx kills the remote command.
func (c *Conn) Run(ctx context.Context, cmd string, stdin io.Reader) (stdout, stderr string, exitCode int, err error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", "", -1, fmt.Errorf("ssh %s: new session: %w", c.addr, err)
	}
	defer func() { _ = sess.Close() }()

	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr, sess.Stdin = &out, &errOut, stdin

	start := time.Now()
	if err := sess.Start(cmd); err != nil {
		return "", "", -1, fmt.Errorf("ssh %s: start: %w", c.addr, err)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(gossh.SIGKILL)
		_ = sess.Close()
		<-done
		return out.String(), errOut.String(), -1, fmt.Errorf("ssh %s: %w", c.addr, ctx.Err())
	}

	exitCode = 0
	var exitErr *gossh.ExitError
	var missing *gossh.ExitMissingError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exitCode, err = exitErr.ExitStatus(), nil
	case errors.As(err, &missing), errors.Is(err, io.EOF):
		exitCode, err = -1, fmt.Errorf("ssh %s: %w", c.addr, ErrDisconnected)
	default:
		exitCode, err = -1, fmt.Errorf("ssh %s: %w", c.addr, err)
	}
	slog.Debug("ssh command finished", "addr", c.addr, "exit", exitCode, "duration", time.Since(start))
	return out.String(), errOut.String(), exitCode, err
}

// Download streams the remote file to w over SFTP without buffering it
// whole, and returns the byte count.
func (c *Conn) Download(ctx context.Context, remote string, w io.Writer) (int64, error) {
	sc, err := c.sftpClient()
	if err != nil {
		return 0, err
	}
	f, err := sc.Open(remote)
	if err != nil {
		return 0, fmt.Errorf("sftp %s: open %s: %w", c.addr, remote, err)
	}
	defer func() { _ = f.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()

	start := time.Now()
	n, err := io.Copy(w, f)
	if ctx.Err() != nil {
		return n, fmt.Errorf("sftp %s: %w", c.addr, ctx.Err())
	}
	if err != nil {
		return n, fmt.Errorf("sftp %s: read %s: %w", c.addr, remote, err)
	}
	slog.Debug("sftp download", "addr", c.addr, "remote", remote, "bytes", n, "duration", time.Since(start))
	return n, nil
}

// Stat returns the size of a remote file over SFTP.
func (c *Conn) Stat(remote string) (int64, error) {
	sc, err := c.sftpClient()
	if err != nil {
		return 0, err
	}
	fi, err := sc.Stat(remote)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func (c *Conn) sftpClient() (*sftp.Client, error) {
	if c.sftp != nil {
		return c.sftp, nil
	}
	sc, err := sftp.NewClient(c.client)
	if err != nil {
		return nil, fmt.Errorf("sftp %s: %w", c.addr, err)
	}
	c.sftp = sc
	return sc, nil
}

// Close closes the connection and any agent socket.
func (c *Conn) Close() error {
	if c.sftp != nil {
		_ = c.sftp.Close()
	}
	if c.agent != nil {
		_ = c.agent.Close()
	}
	return c.client.Close()
}
