package ssh

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Client is a stateless, thread-safe SSH client. Multiple goroutines may each
// hold their own Client instance. The underlying ssh.Client is created lazily
// on the first call to Run or Download.
type Client struct {
	Host       string
	User       string
	Pass       string
	KeyFile    string
	Port       int
	Timeout    time.Duration
	RequestPTY bool // set true for MikroTik / interactive commands

	client *ssh.Client
}

// connect establishes the SSH connection using the configured auth methods.
// Auth order: identity file → SSH agent → password.
func (c *Client) connect() error {
	if c.client != nil {
		return nil
	}

	var authMethods []ssh.AuthMethod

	// 1. Identity file
	if c.KeyFile != "" {
		expanded := expandHome(c.KeyFile)
		keyBytes, err := os.ReadFile(expanded)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(keyBytes)
			if err == nil {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
			}
		}
	}

	// 2. SSH agent (SSH_AUTH_SOCK)
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		agentConn, err := net.Dial("unix", sock)
		if err == nil {
			agentClient := agent.NewClient(agentConn)
			authMethods = append(authMethods, ssh.PublicKeysCallback(agentClient.Signers))
		}
	}

	// 3. Password
	if c.Pass != "" {
		authMethods = append(authMethods, ssh.Password(c.Pass))
	}

	if len(authMethods) == 0 {
		return fmt.Errorf("no authentication methods available for %s", c.Host)
	}

	hostKeyCallback := ssh.InsecureIgnoreHostKey() // TODO: use knownhosts.New for production
	_ = knownhosts.New                             // imported to allow future enablement

	config := &ssh.ClientConfig{
		User:            c.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.Timeout,
	}

	port := c.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(c.Host, fmt.Sprintf("%d", port))

	// Dial with explicit timeout at the TCP level.
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	tcpConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	if err != nil {
		tcpConn.Close()
		return fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	c.client = ssh.NewClient(ncc, chans, reqs)
	return nil
}

// Run executes cmd on the remote host and returns stdout, stderr, exit code,
// and any transport-level error. A non-zero exit code from the remote command
// is not returned as an error — callers should inspect exitCode instead.
func (c *Client) Run(cmd string) (stdout, stderr string, exitCode int, err error) {
	if err = c.connect(); err != nil {
		return
	}

	session, err := c.client.NewSession()
	if err != nil {
		err = fmt.Errorf("new session: %w", err)
		return
	}
	defer session.Close()

	if c.RequestPTY {
		modes := ssh.TerminalModes{
			ssh.ECHO:          0,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if ptyErr := session.RequestPty("vt100", 40, 80, modes); ptyErr != nil {
			err = fmt.Errorf("request pty: %w", ptyErr)
			return
		}
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	runErr := session.Run(cmd)

	stdout = string(bytes.TrimRight(stdoutBuf.Bytes(), "\r\n"))
	stderr = string(bytes.TrimRight(stderrBuf.Bytes(), "\r\n"))

	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
			// Non-zero exit is not a transport error.
			err = nil
		} else {
			err = runErr
		}
	}
	return
}

// RunWithPTY is like Run but always requests a PTY. Used for MikroTik commands.
func (c *Client) RunWithPTY(cmd string) (stdout, stderr string, exitCode int, err error) {
	old := c.RequestPTY
	c.RequestPTY = true
	defer func() { c.RequestPTY = old }()
	return c.Run(cmd)
}

// Download copies a remote file to a local path using the cat command over SSH.
// NOTE: Binary files with NUL bytes may not transfer correctly; use SFTP for
// binary transfers.
func (c *Client) Download(remote, local string) error {
	if err := c.connect(); err != nil {
		return err
	}

	session, err := c.client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	f, err := os.Create(local)
	if err != nil {
		return fmt.Errorf("create local file %s: %w", local, err)
	}
	defer f.Close()

	session.Stdout = f
	var stderrBuf bytes.Buffer
	session.Stderr = &stderrBuf

	if err := session.Run("cat " + remote); err != nil {
		_ = os.Remove(local)
		return fmt.Errorf("download %s: %s: %w", remote, stderrBuf.String(), err)
	}
	return nil
}

// UploadScript sends script content via stdin and runs it with bash -s.
// Returns stdout, stderr, exit code, error — same semantics as Run.
func (c *Client) UploadScript(script []byte) (stdout, stderr string, exitCode int, err error) {
	if err = c.connect(); err != nil {
		return
	}

	session, err := c.client.NewSession()
	if err != nil {
		err = fmt.Errorf("new session: %w", err)
		return
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(script)

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	runErr := session.Run("bash -s")

	stdout = string(bytes.TrimRight(stdoutBuf.Bytes(), "\r\n"))
	stderr = string(bytes.TrimRight(stderrBuf.Bytes(), "\r\n"))

	if runErr != nil {
		if exitErr, ok := runErr.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
			err = nil
		} else {
			err = runErr
		}
	}
	return
}

// Close terminates the underlying SSH connection.
func (c *Client) Close() {
	if c.client != nil {
		c.client.Close()
		c.client = nil
	}
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(path string) string {
	if len(path) > 0 && path[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			return home + path[1:]
		}
	}
	return path
}

