// Package webui serves the loopback config editor for `mrsh ui`.
//
// The server only edits config: it never resolves secrets, opens SSH
// connections or runs commands. Binding to loopback does not stop web pages
// in the user's own browser from sending it requests, so every API call
// must carry a per-launch token in a custom header (which cross-site pages
// cannot set without a CORS preflight this server never grants), the Host
// header must name the bound address (defeating DNS rebinding), and
// mutations must be JSON.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
)

//go:embed dist
var dist embed.FS

// TokenHeader carries the per-launch token on API requests.
const TokenHeader = "X-Mrsh-Token"

// Server is one `mrsh ui` session.
type Server struct {
	Store config.ConfigStore
	Token string
	addr  string // host:port actually bound

	quitMu   sync.Mutex
	quit     chan struct{} // closed when the page asks the server to exit
	quitOnce sync.Once
}

// quitChan returns the exit channel, creating it on first use.
func (s *Server) quitChan() chan struct{} {
	s.quitMu.Lock()
	defer s.quitMu.Unlock()
	if s.quit == nil {
		s.quit = make(chan struct{})
	}
	return s.quit
}

// requestQuit asks Serve to shut down; later calls do nothing.
func (s *Server) requestQuit() { s.quitOnce.Do(func() { close(s.quitChan()) }) }

// CheckLoopback rejects listen addresses that are not loopback.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--addr %q is not a loopback address; mrsh ui only listens on 127.0.0.1, ::1 or localhost", addr)
}

// Listen binds addr (loopback only) and returns a Server with a fresh token.
func Listen(store config.ConfigStore, addr string) (*Server, net.Listener, error) {
	if err := CheckLoopback(addr); err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	return &Server{Store: store, Token: hex.EncodeToString(tok), addr: ln.Addr().String()}, ln, nil
}

// URL is the address to open; the page moves the token out of the URL on load.
func (s *Server) URL() string { return "http://" + s.addr + "/#token=" + s.Token }

// Serve runs until ctx ends or the page requests exit, then shuts down
// gracefully (in-flight replies, such as the exit request's, are sent).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	case <-s.quitChan():
		slog.Debug("ui exit requested from the page")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}

// Handler returns the full handler: static page plus guarded API.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(dist, "dist")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	s.routes(mux)
	return s.guard(mux)
}

// allowedHosts are the Host header values that name this server.
func (s *Server) allowedHosts() map[string]bool {
	_, port, _ := net.SplitHostPort(s.addr)
	return map[string]bool{
		s.addr:              true,
		"localhost:" + port: true,
		"127.0.0.1:" + port: true,
		"[::1]:" + port:     true,
	}
}

func (s *Server) guard(next http.Handler) http.Handler {
	hosts := s.allowedHosts()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")

		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		// Page files must be revalidated so a new mrsh build never runs
		// with a stale app.js cached from an earlier session.
		h.Set("Cache-Control", "no-cache")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), []byte(s.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or wrong session token; reopen the URL printed by mrsh ui")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !hosts[strings.TrimPrefix(origin, "http://")] {
				writeError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete &&
				!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "requests must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// logRequest is a debug hook for handlers.
func logRequest(r *http.Request, status int, start time.Time) {
	slog.Debug("ui request", "method", r.Method, "path", r.URL.Path, "status", status, "duration", time.Since(start))
}

var errBadRequest = errors.New("bad request")
