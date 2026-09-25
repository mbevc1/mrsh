package cmd

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/webui"
)

// syncBuffer is a goroutine-safe output sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestUICommand(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: web01, host: 10.0.0.1}\n")
	opened := make(chan string, 1)
	old := openBrowser
	openBrowser = func(u string) error { opened <- u; return nil }
	t.Cleanup(func() { openBrowser = old })

	root := newRootCmd()
	var out syncBuffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"-f", cfg, "ui", "--addr", "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	var url string
	select {
	case url = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatalf("browser never opened; output:\n%s", out.String())
	}
	m := regexp.MustCompile(`^http://(127\.0\.0\.1:\d+)/#token=([0-9a-f]{64})$`).FindStringSubmatch(url)
	if m == nil {
		t.Fatalf("url = %q", url)
	}
	req, _ := http.NewRequest("GET", "http://"+m[1]+"/api/config", nil)
	req.Header.Set(webui.TokenHeader, m[2])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "web01") {
		t.Errorf("config: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(out.String(), "open "+url) {
		t.Errorf("URL not printed:\n%s", out.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ui exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ui did not stop on cancel")
	}
}

func TestUIRefusesNonLoopbackAndMissingConfig(t *testing.T) {
	cfg := writeConfig(t, "hosts: []\n")
	if _, err := execRoot(t, "-f", cfg, "ui", "--no-open", "--addr", "0.0.0.0:7171"); err == nil || !strings.Contains(err.Error(), "not a loopback") {
		t.Errorf("err = %v", err)
	}
	if _, err := execRoot(t, "-f", "/nonexistent/hosts.yaml", "ui", "--no-open"); err == nil || !strings.Contains(err.Error(), "hosts init") {
		t.Errorf("err = %v", err)
	}
}
