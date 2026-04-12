package ssh

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
)

// startTestServer starts a minimal SSH server on a random local port.
// It accepts any password and echoes the command output.
func startTestServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := &ssh.Server{
		Handler: func(s ssh.Session) {
			cmd := s.Command()
			if len(cmd) > 0 {
				switch cmd[0] {
				case "echo":
					s.Write([]byte(strings.Join(cmd[1:], " ") + "\n"))
				case "exit":
					// Return non-zero exit.
					s.Exit(1)
				default:
					s.Write([]byte("unknown\n"))
				}
			}
			s.Exit(0)
		},
		PasswordHandler: func(ctx ssh.Context, pass string) bool {
			return true // accept any password
		},
	}

	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})

	return ln.Addr().String()
}

func TestRun(t *testing.T) {
	addr := startTestServer(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port := 22
	if _, err := net.LookupPort("tcp", portStr); err == nil {
		net.LookupPort("tcp", portStr)
	}
	// Parse port from string.
	var p int
	if _, err := net.ResolveTCPAddr("tcp", addr); err == nil {
		tcpAddr, _ := net.ResolveTCPAddr("tcp", addr)
		p = tcpAddr.Port
	}
	if p == 0 {
		p = port
	}

	client := &Client{
		Host:    host,
		User:    "testuser",
		Pass:    "testpass",
		Port:    p,
		Timeout: 5 * time.Second,
	}

	t.Run("echo command", func(t *testing.T) {
		stdout, stderr, code, err := client.Run("echo hello world")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		_ = stderr
		_ = stdout
		// Note: the test server uses cmd[0]=="echo" handler
	})

	t.Run("connection is reused", func(t *testing.T) {
		if client.client == nil {
			t.Skip("client not connected after first Run")
		}
		firstClient := client.client
		_, _, _, _ = client.Run("echo again")
		if client.client != firstClient {
			t.Error("expected connection to be reused across calls")
		}
	})

	t.Run("close clears client", func(t *testing.T) {
		client.Close()
		if client.client != nil {
			t.Error("Close() should set client to nil")
		}
	})
}

func TestExpandHome(t *testing.T) {
	result := expandHome("~/.ssh/id_rsa")
	if strings.HasPrefix(result, "~") {
		t.Errorf("expandHome should replace ~, got %q", result)
	}
	if !strings.HasSuffix(result, "/.ssh/id_rsa") {
		t.Errorf("expandHome should preserve path suffix, got %q", result)
	}

	// Non-home path should be unchanged.
	unchanged := expandHome("/etc/ssh/key")
	if unchanged != "/etc/ssh/key" {
		t.Errorf("expandHome should not modify absolute paths, got %q", unchanged)
	}
}
