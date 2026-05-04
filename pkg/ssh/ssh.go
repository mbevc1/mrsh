package ssh

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
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
	Debug      func(format string, args ...interface{})

	client *ssh.Client
}

func (c *Client) debugf(format string, args ...interface{}) {
	if c.Debug != nil {
		c.Debug(format, args...)
	}
}

// connect establishes the SSH connection using the configured auth methods.
// Auth order: identity file → SSH agent → password.
func (c *Client) connect() error {
	if c.client != nil {
		return nil
	}

	var authMethods []ssh.AuthMethod
	var authNames []string

	// 1. Identity file
	if c.KeyFile != "" {
		expanded := expandHome(c.KeyFile)
		keyBytes, err := os.ReadFile(expanded)
		if err != nil {
			c.debugf("identity file %s: %v", expanded, err)
		} else {
			signer, err := ssh.ParsePrivateKey(keyBytes)
			if err != nil {
				c.debugf("parse private key %s: %v", expanded, err)
			} else {
				authMethods = append(authMethods, ssh.PublicKeys(signer))
				authNames = append(authNames, "key:"+expanded)
			}
		}
	}

	// 2. SSH agent (SSH_AUTH_SOCK)
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		agentConn, err := net.Dial("unix", sock)
		if err != nil {
			c.debugf("ssh agent dial: %v", err)
		} else {
			agentClient := agent.NewClient(agentConn)
			authMethods = append(authMethods, ssh.PublicKeysCallback(agentClient.Signers))
			authNames = append(authNames, "agent")
		}
	}

	// 3. Password
	if c.Pass != "" {
		authMethods = append(authMethods, ssh.Password(c.Pass))
		authNames = append(authNames, "password")
	}

	if len(authMethods) == 0 {
		return fmt.Errorf("no authentication methods available for %s", c.Host)
	}
	c.debugf("auth methods for %s: %v", c.Host, authNames)

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
	c.debugf("dialing %s as %s (timeout=%s)", addr, c.User, timeout)
	tcpConn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		c.debugf("dial %s failed: %v", addr, err)
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, config)
	if err != nil {
		tcpConn.Close()
		c.debugf("ssh handshake %s failed: %v", addr, err)
		return fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	c.client = ssh.NewClient(ncc, chans, reqs)
	c.debugf("connected to %s", addr)
	return nil
}

// Run executes cmd on the remote host and returns stdout, stderr, exit code,
// and any transport-level error. A non-zero exit code from the remote command
// is not returned as an error — callers should inspect exitCode instead.
func (c *Client) Run(cmd string) (stdout, stderr string, exitCode int, err error) {
	if err = c.connect(); err != nil {
		return
	}
	c.debugf("%s exec (pty=%t): %s", c.Host, c.RequestPTY, cmd)

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

	stdout = string(bytes.Trim(stdoutBuf.Bytes(), "\r\n"))
	stderr = string(bytes.Trim(stderrBuf.Bytes(), "\r\n"))

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

// RunWithInput types cmd into an interactive shell session, waits for any of
// the supplied prompt patterns to appear in stdout, then writes the response.
// This is needed for commands like RouterOS "/system reboot" which prints
// "Reboot, yes? [y/N]:" and reads from a real terminal — an exec channel
// with a pre-buffered stdin reader does not work because the server reaches
// stdin EOF before printing the prompt.
//
// Input is the bytes to type once a prompt is matched. Prompts is the list of
// substrings to look for; if nil, defaults to common confirmation patterns.
// Always uses a PTY regardless of c.RequestPTY.
func (c *Client) RunWithInput(cmd string, input []byte, prompts ...string) (stdout, stderr string, exitCode int, err error) {
	if err = c.connect(); err != nil {
		return
	}
	if len(prompts) == 0 {
		prompts = []string{"[y/N]", "[Y/n]", "(y/n)", "yes?"}
	}
	c.debugf("%s shell-interact (input=%d byte(s)): %s", c.Host, len(input), cmd)

	session, err := c.client.NewSession()
	if err != nil {
		err = fmt.Errorf("new session: %w", err)
		return
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if ptyErr := session.RequestPty("vt100", 40, 80, modes); ptyErr != nil {
		err = fmt.Errorf("request pty: %w", ptyErr)
		return
	}

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		err = fmt.Errorf("stdin pipe: %w", err)
		return
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		err = fmt.Errorf("stdout pipe: %w", err)
		return
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		err = fmt.Errorf("stderr pipe: %w", err)
		return
	}

	var (
		bufMu     sync.Mutex
		stdoutBuf bytes.Buffer
		stderrBuf bytes.Buffer
	)
	go streamTo(stdoutPipe, &stdoutBuf, &bufMu)
	go streamTo(stderrPipe, &stderrBuf, &bufMu)

	if err = session.Shell(); err != nil {
		err = fmt.Errorf("shell: %w", err)
		return
	}

	// Type the command followed by Enter.
	if _, err = fmt.Fprintln(stdinPipe, cmd); err != nil {
		err = fmt.Errorf("write cmd: %w", err)
		return
	}

	// Wait for any prompt pattern to appear, up to 5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		bufMu.Lock()
		out := stdoutBuf.String()
		bufMu.Unlock()
		matched := false
		for _, p := range prompts {
			if strings.Contains(out, p) {
				matched = true
				break
			}
		}
		if matched {
			c.debugf("%s prompt matched after %d byte(s) of stdout", c.Host, len(out))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Type the response. Errors here are usually benign — the device may
	// already be tearing the connection down (e.g. on /system reboot).
	if _, werr := stdinPipe.Write(input); werr != nil {
		c.debugf("%s write input: %v", c.Host, werr)
	}

	runErr := session.Wait()

	bufMu.Lock()
	stdout = string(bytes.Trim(stdoutBuf.Bytes(), "\r\n"))
	stderr = string(bytes.Trim(stderrBuf.Bytes(), "\r\n"))
	bufMu.Unlock()

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

// streamTo copies r into buf until r returns an error (typically EOF when the
// remote side closes the channel).
func streamTo(r interface{ Read([]byte) (int, error) }, buf *bytes.Buffer, mu *sync.Mutex) {
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			mu.Lock()
			buf.Write(chunk[:n])
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
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

	stdout = string(bytes.Trim(stdoutBuf.Bytes(), "\r\n"))
	stderr = string(bytes.Trim(stderrBuf.Bytes(), "\r\n"))

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

