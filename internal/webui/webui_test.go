package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbevc1/mrsh/internal/config"
)

const seed = `# managed by mrsh
defaults:
  user: deploy
hosts:
  - name: web01
    host: 10.0.0.1
    group: web
    user: admin
    pass: s3cret-literal
  - name: db01
    host: 10.0.0.10
    group: db
    pass_arn: arn:aws:ssm:eu-west-1:123456789012:parameter/db01
`

type fixture struct {
	t    *testing.T
	srv  *Server
	h    http.Handler
	path string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Store: &config.LocalStore{Path: path}, Token: "tok123", addr: "127.0.0.1:7171"}
	return &fixture{t: t, srv: srv, h: srv.Handler(), path: path}
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func host(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }

func (f *fixture) do(method, path, body string, opts ...reqOpt) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = "127.0.0.1:7171"
	r.Header.Set(TokenHeader, "tok123")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (f *fixture) version() string {
	f.t.Helper()
	w, out := f.do("GET", "/api/config", "")
	if w.Code != 200 {
		f.t.Fatalf("GET config: %d %s", w.Code, w.Body)
	}
	return out["version"].(string)
}

func (f *fixture) config() *config.Config {
	f.t.Helper()
	raw, _ := os.ReadFile(f.path)
	c, err := config.Parse(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestGuard(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		opts   []reqOpt
		want   int
	}{
		{"no token", "GET", "/api/config", "", []reqOpt{header(TokenHeader, "")}, 401},
		{"wrong token", "GET", "/api/config", "", []reqOpt{header(TokenHeader, "nope")}, 401},
		{"dns rebinding host", "GET", "/api/config", "", []reqOpt{host("evil.example:7171")}, 403},
		{"rebinding host on static page", "GET", "/", "", []reqOpt{host("evil.example:7171")}, 403},
		{"cross origin", "GET", "/api/config", "", []reqOpt{header("Origin", "http://evil.example")}, 403},
		{"form post", "POST", "/api/hosts", "name=x", []reqOpt{header("Content-Type", "application/x-www-form-urlencoded")}, 415},
		{"same origin ok", "GET", "/api/config", "", []reqOpt{header("Origin", "http://127.0.0.1:7171")}, 200},
		{"localhost host ok", "GET", "/api/health", "", []reqOpt{host("localhost:7171")}, 200},
	}
	for _, tt := range tests {
		w, _ := f.do(tt.method, tt.path, tt.body, tt.opts...)
		if w.Code != tt.want {
			t.Errorf("%s: status %d, want %d (%s)", tt.name, w.Code, tt.want, w.Body)
		}
	}
}

func TestStaticPageAndHeaders(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		w, _ := f.do("GET", p, "", header(TokenHeader, ""))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", p, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache so upgrades never run a stale app.js", p, w.Header().Get("Cache-Control"))
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") || w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: security headers missing: %v", p, w.Header())
		}
	}
}

func TestGetConfigNeverLeaksSecrets(t *testing.T) {
	t.Setenv("DB_PASS", "resolved-env-secret")
	f := newFixture(t)
	w, out := f.do("GET", "/api/config", "")
	body := w.Body.String()
	for _, secret := range []string{"s3cret-literal", "admin", "resolved-env-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("GET /api/config leaks %q:\n%s", secret, body)
		}
	}
	hosts := out["hosts"].([]any)
	web := hosts[0].(map[string]any)
	if pass := web["pass"].(map[string]any); pass["kind"] != "plain" || pass["set"] != true || pass["value"] != "" {
		t.Errorf("literal pass view = %v", pass)
	}
	db := hosts[1].(map[string]any)
	if pass := db["pass"].(map[string]any); pass["kind"] != "arn" || !strings.Contains(pass["value"].(string), "parameter/db01") {
		t.Errorf("arn pass view = %v", pass)
	}
	if w.Header().Get("ETag") != out["version"] || out["version"] == "" {
		t.Errorf("version %v vs ETag %q", out["version"], w.Header().Get("ETag"))
	}
	if g := out["groups"].([]any); len(g) != 2 {
		t.Errorf("groups = %v", g)
	}
}

func TestAddHostValidation(t *testing.T) {
	f := newFixture(t)
	v := f.version()
	tests := []struct {
		name string
		body string
		want int
		msg  string
	}{
		{"duplicate name", `{"name":"web01","host":"10.0.0.9","user":{"kind":"unset"},"pass":{"kind":"unset"}}`, 409, "already exists"},
		{"bad arn", `{"name":"x","host":"h","user":{"kind":"unset"},"pass":{"kind":"arn","value":"arn:aws:s3:::bucket"}}`, 422, "pass_arn"},
		{"bad port", `{"name":"x","host":"h","port":70000,"user":{"kind":"unset"},"pass":{"kind":"unset"}}`, 422, "out of range"},
		{"missing host", `{"name":"x","host":"","user":{"kind":"unset"},"pass":{"kind":"unset"}}`, 422, "host is required"},
		{"empty literal on new host", `{"name":"x","host":"h","user":{"kind":"unset"},"pass":{"kind":"plain","value":""}}`, 422, "enter a literal"},
		{"unknown field", `{"name":"x","host":"h","pass_env":"X"}`, 400, "unknown field"},
		{"unknown kind", `{"name":"x","host":"h","user":{"kind":"unset"},"pass":{"kind":"magic","value":"v"}}`, 422, "unknown kind"},
	}
	for _, tt := range tests {
		w, _ := f.do("POST", "/api/hosts", tt.body, header("If-Match", v))
		if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.msg) {
			t.Errorf("%s: %d %s, want %d containing %q", tt.name, w.Code, w.Body, tt.want, tt.msg)
		}
	}
	if raw, _ := os.ReadFile(f.path); string(raw) != seed {
		t.Error("rejected requests changed the file")
	}
}

func TestVersionGuard(t *testing.T) {
	f := newFixture(t)
	body := `{"name":"new","host":"10.0.0.5","user":{"kind":"unset"},"pass":{"kind":"env","value":"NEW_PASS"}}`
	if w, _ := f.do("POST", "/api/hosts", body); w.Code != 428 {
		t.Errorf("missing If-Match: %d", w.Code)
	}
	v := f.version()

	// The CLI edits the file after the UI loaded it.
	store := &config.LocalStore{Path: f.path}
	if err := config.Mutate(context.Background(), store, func(d *config.Document, _ *config.Config) error {
		return d.AddHost(config.Host{Name: "from-cli", Host: "10.0.0.7"})
	}); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.do("POST", "/api/hosts", body, header("If-Match", v)); w.Code != 412 {
		t.Fatalf("stale If-Match: %d %s", w.Code, w.Body)
	}
	// After reloading, the UI sees the CLI's host and can save.
	_, out := f.do("GET", "/api/config", "")
	if !strings.Contains(mustJSON(t, out), "from-cli") {
		t.Error("UI does not reflect the CLI edit")
	}
	w, res := f.do("POST", "/api/hosts", body, header("If-Match", out["version"].(string)))
	if w.Code != 200 || res["version"] == "" || res["version"] == out["version"] {
		t.Fatalf("save: %d %v", w.Code, res)
	}
	// And the CLI sees the UI's host.
	c := f.config()
	if len(c.Hosts) != 4 || c.Hosts[3].Name != "new" || c.Hosts[3].PassEnv != "NEW_PASS" {
		t.Errorf("hosts = %+v", c.Hosts)
	}
	if raw, _ := os.ReadFile(f.path); !strings.Contains(string(raw), "# managed by mrsh") {
		t.Error("comments lost")
	}
}

func TestUpdateKeepsLiteralAndRenames(t *testing.T) {
	f := newFixture(t)
	v := f.version()
	// kind plain with an empty value keeps the stored literal; group changes.
	w, res := f.do("PUT", "/api/hosts/web01",
		`{"name":"web01","host":"10.0.0.1","group":"edge","user":{"kind":"plain","value":""},"pass":{"kind":"plain","value":""}}`,
		header("If-Match", v))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	h := f.config().Hosts[0]
	if h.Pass != "s3cret-literal" || h.User != "admin" || h.Group != "edge" {
		t.Errorf("host = %+v", h)
	}

	// Switching to env drops the literal; renaming keeps the position.
	w, res = f.do("PUT", "/api/hosts/web01",
		`{"name":"edge01","host":"10.0.0.1","group":"edge","user":{"kind":"unset"},"pass":{"kind":"env","value":"EDGE_PASS"}}`,
		header("If-Match", res["version"].(string)))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	h = f.config().Hosts[0]
	if h.Name != "edge01" || h.Pass != "" || h.PassEnv != "EDGE_PASS" || h.User != "" {
		t.Errorf("host = %+v", h)
	}

	// Renaming onto an existing name is refused.
	w, _ = f.do("PUT", "/api/hosts/edge01",
		`{"name":"db01","host":"10.0.0.1","user":{"kind":"unset"},"pass":{"kind":"unset"}}`,
		header("If-Match", res["version"].(string)))
	if w.Code != 409 {
		t.Errorf("rename onto existing: %d", w.Code)
	}
	if w, _ := f.do("PUT", "/api/hosts/nope", `{"name":"nope","host":"h","user":{},"pass":{}}`, header("If-Match", f.version())); w.Code != 404 {
		t.Errorf("update missing: %d", w.Code)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	w, _ := f.do("DELETE", "/api/hosts/db01", "", header("If-Match", f.version()))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := f.config(); len(c.Hosts) != 1 || c.Hosts[0].Name != "web01" {
		t.Errorf("hosts = %+v", c.Hosts)
	}
	if w, _ := f.do("DELETE", "/api/hosts/db01", "", header("If-Match", f.version())); w.Code != 404 {
		t.Errorf("second delete: %d", w.Code)
	}
}

func TestValidateDoesNotSave(t *testing.T) {
	f := newFixture(t)
	w, out := f.do("POST", "/api/validate",
		`{"original":"","host":{"name":"web01","host":"h","user":{"kind":"unset"},"pass":{"kind":"arn","value":"bad"}}}`)
	if w.Code != 200 || out["ok"] != false {
		t.Fatalf("%d %v", w.Code, out)
	}
	msgs := mustJSON(t, out["errors"])
	if !strings.Contains(msgs, "duplicate name") || !strings.Contains(msgs, "pass_arn") {
		t.Errorf("errors = %s", msgs)
	}
	_, out = f.do("POST", "/api/validate", `{"original":"web01","host":{"name":"web01","host":"h2","user":{"kind":"plain"},"pass":{"kind":"plain"}}}`)
	if out["ok"] != true {
		t.Errorf("editing web01 in place should validate: %v", out)
	}
	if raw, _ := os.ReadFile(f.path); string(raw) != seed {
		t.Error("validate changed the file")
	}
}

func TestDefaultsAndCommands(t *testing.T) {
	f := newFixture(t)
	w, res := f.do("PUT", "/api/defaults",
		`{"port":2222,"timeout":10,"parallel":4,"output":"json","debug":false,"identity_file":"~/.ssh/k","known_hosts":"","host_key_policy":"strict","user":{"kind":"plain","value":""},"pass":{"kind":"env","value":"DEF_PASS"}}`,
		header("If-Match", f.version()))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d := f.config().Defaults
	if d.Port != 2222 || d.Parallel != 4 || d.HostKeyPolicy != "strict" || d.User != "deploy" || d.PassEnv != "DEF_PASS" {
		t.Errorf("defaults = %+v", d)
	}
	if w, _ := f.do("PUT", "/api/defaults", `{"output":"yaml","user":{},"pass":{}}`, header("If-Match", res["version"].(string))); w.Code != 422 {
		t.Errorf("bad output: %d", w.Code)
	}

	w, _ = f.do("PUT", "/api/commands", `["uptime", "  ", "df -h"]`, header("If-Match", f.version()))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if c := f.config(); strings.Join(c.Commands, "|") != "uptime|df -h" {
		t.Errorf("commands = %q", c.Commands)
	}
	f.do("PUT", "/api/commands", `[]`, header("If-Match", f.version()))
	if c := f.config(); c.Commands != nil {
		t.Errorf("commands not cleared: %q", c.Commands)
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:7171": true, "[::1]:7171": true, "localhost:7171": true, "127.0.0.2:1": true,
		"0.0.0.0:7171": false, ":7171": false, "192.168.1.5:7171": false, "example.com:80": false, "nonsense": false,
	} {
		if err := CheckLoopback(addr); (err == nil) != ok {
			t.Errorf("CheckLoopback(%q) = %v", addr, err)
		}
	}
}

func TestListenAndServe(t *testing.T) {
	store := &config.LocalStore{Path: filepath.Join(t.TempDir(), "h.yaml")}
	_ = store.Save(context.Background(), []byte("hosts: []\n"), "")
	srv, ln, err := Listen(store, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Token) != 64 || !strings.Contains(srv.URL(), "#token="+srv.Token) {
		t.Errorf("token %q url %q", srv.Token, srv.URL())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	req, _ := http.NewRequest("GET", "http://"+ln.Addr().String()+"/api/health", nil)
	req.Header.Set(TokenHeader, srv.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if _, _, err := Listen(store, "0.0.0.0:0"); err == nil {
		t.Error("non-loopback listen allowed")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShutdownEndpoint(t *testing.T) {
	f := newFixture(t)
	if w, _ := f.do("POST", "/api/shutdown", "{}", header(TokenHeader, "")); w.Code != 401 {
		t.Errorf("no token: %d", w.Code)
	}
	if w, _ := f.do("POST", "/api/shutdown", "x", header("Content-Type", "text/plain")); w.Code != 415 {
		t.Errorf("text/plain: %d", w.Code)
	}
	select {
	case <-f.srv.quitChan():
		t.Fatal("rejected requests triggered shutdown")
	default:
	}
	w, out := f.do("POST", "/api/shutdown", "{}")
	if w.Code != 200 || out["ok"] != true {
		t.Fatalf("shutdown: %d %v", w.Code, out)
	}
	select {
	case <-f.srv.quitChan():
	default:
		t.Fatal("quit channel not closed")
	}
	// A second request is harmless.
	if w, _ := f.do("POST", "/api/shutdown", "{}"); w.Code != 200 {
		t.Errorf("second shutdown: %d", w.Code)
	}
}

func TestServeStopsOnShutdownRequest(t *testing.T) {
	store := &config.LocalStore{Path: filepath.Join(t.TempDir(), "h.yaml")}
	_ = store.Save(context.Background(), []byte("hosts: []\n"), "")
	srv, ln, err := Listen(store, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), ln) }()

	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/api/shutdown", strings.NewReader("{}"))
	req.Header.Set(TokenHeader, srv.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("reply: %d %s", resp.StatusCode, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown request")
	}
}
