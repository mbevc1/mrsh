package webui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mbevc1/mrsh/internal/config"
)

// memSecrets is an in-memory SecretWriter.
type memSecrets struct {
	mu     sync.Mutex
	params map[string]string
}

func (m *memSecrets) PutSecret(_ context.Context, name, value string, overwrite bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.params[name]; ok && !overwrite {
		return "", fmt.Errorf("%s: %w", name, config.ErrSecretExists)
	}
	m.params[name] = value
	return "arn:aws:ssm:eu-west-1:123456789012:parameter" + name, nil
}

func (m *memSecrets) DeleteSecret(_ context.Context, arn string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, _ := config.SSMParamName(arn)
	delete(m.params, name)
	return nil
}

func TestStoreSecretsInSSM(t *testing.T) {
	f := newFixture(t)
	mem := &memSecrets{params: map[string]string{}}
	mut := func(method, path, body string) (int, string) {
		t.Helper()
		w, _ := f.do(method, path, body, header("If-Match", f.version()))
		return w.Code, w.Body.String()
	}

	// Not wired up: refused, nothing stored.
	if code, body := mut("POST", "/api/hosts", `{"name":"rb9","host":"10.0.0.9","pass":{"kind":"ssm","value":"pw-9"}}`); code != 422 || !strings.Contains(body, "not available") {
		t.Fatalf("no writer: %d %s", code, body)
	}
	f.srv.Secrets = mem
	f.h = f.srv.Handler()

	// An edit the config refuses stores nothing.
	if code, body := mut("POST", "/api/hosts", `{"name":"rb9","host":"http://bad","pass":{"kind":"ssm","value":"pw-9"}}`); code != 422 || len(mem.params) != 0 {
		t.Fatalf("invalid host: %d %s stored=%v", code, body, mem.params)
	}

	if code, body := mut("POST", "/api/hosts", `{"name":"rb9","host":"10.0.0.9","pass":{"kind":"ssm","value":"pw-9"}}`); code != 200 {
		t.Fatalf("add: %d %s", code, body)
	}
	if mem.params["/mrsh/hosts/rb9/pass"] != "pw-9" {
		t.Fatalf("params = %v", mem.params)
	}
	h, _ := find(f.config(), "rb9")
	if h.PassARN != "arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/hosts/rb9/pass" || h.Pass != "" {
		t.Errorf("host = %+v", h)
	}
	if raw, _ := os.ReadFile(f.path); strings.Contains(string(raw), "pw-9") {
		t.Error("the value reached the config file")
	}
	if _, out := f.do("GET", "/api/config", ""); strings.Contains(fmt.Sprint(out), "pw-9") {
		t.Error("the value was sent back to the page")
	}

	// An existing parameter needs the overwrite box; a custom name is used as given.
	if code, body := mut("PUT", "/api/hosts/rb9", `{"name":"rb9","host":"10.0.0.9","pass":{"kind":"ssm","value":"pw-new"}}`); code != 422 || !strings.Contains(body, "Replace an existing parameter") {
		t.Fatalf("exists: %d %s", code, body)
	}
	if code, body := mut("PUT", "/api/hosts/rb9", `{"name":"rb9","host":"10.0.0.9","pass":{"kind":"ssm","value":"pw-new","overwrite":true},"user":{"kind":"ssm","value":"ops","name":"/ops/rb9-user"}}`); code != 200 {
		t.Fatalf("overwrite: %d %s", code, body)
	}
	if mem.params["/mrsh/hosts/rb9/pass"] != "pw-new" || mem.params["/ops/rb9-user"] != "ops" {
		t.Errorf("params = %v", mem.params)
	}

	// Defaults: the default name follows the prefix being saved.
	if code, body := mut("PUT", "/api/defaults", `{"ssm_prefix":"/team-a","user":{"kind":"ssm","value":"admin"}}`); code != 200 {
		t.Fatalf("defaults: %d %s", code, body)
	}
	if d := f.config().Defaults; mem.params["/team-a/defaults/user"] != "admin" || d.SSMPrefix != "/team-a" || !strings.HasSuffix(d.UserARN, "/team-a/defaults/user") {
		t.Errorf("defaults = %+v params=%v", d, mem.params)
	}

	// Credential sets, and a bad parameter name.
	if code, body := mut("POST", "/api/credentials", `{"name":"lab","pass":{"kind":"ssm","value":"lab-pw"}}`); code != 200 || mem.params["/team-a/creds/lab/pass"] != "lab-pw" {
		t.Fatalf("credential: %d %s", code, body)
	}
	if code, body := mut("PUT", "/api/credentials/lab", `{"pass":{"kind":"ssm","value":"x","name":"/aws/x"}}`); code != 422 || !strings.Contains(body, "reserved") {
		t.Errorf("reserved name: %d %s", code, body)
	}
}

func TestRemoveDeletesSSMParameters(t *testing.T) {
	f := newFixture(t)
	mem := &memSecrets{params: map[string]string{}}
	f.srv.Secrets = mem
	f.h = f.srv.Handler()
	mut := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		w, out := f.do(method, path, body, header("If-Match", f.version()))
		return w.Code, out
	}
	for _, body := range []string{
		`{"name":"lab","pass":{"kind":"ssm","value":"lab-pw"}}`,
	} {
		if code, out := mut("POST", "/api/credentials", body); code != 200 {
			t.Fatalf("credential: %d %v", code, out)
		}
	}
	labARN := "arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/creds/lab/pass"
	for _, body := range []string{
		`{"name":"rb8","host":"10.0.0.8","pass":{"kind":"ssm","value":"pw-8"}}`,
		`{"name":"rb7","host":"10.0.0.7","pass":{"kind":"arn","value":"` + labARN + `"}}`, // shares lab's parameter
	} {
		if code, out := mut("POST", "/api/hosts", body); code != 200 {
			t.Fatalf("host: %d %v", code, out)
		}
	}

	// Without the flag nothing in SSM changes.
	if code, out := mut("DELETE", "/api/hosts/rb8", ""); code != 200 || out["secrets"] != nil || mem.params["/mrsh/hosts/rb8/pass"] == "" {
		t.Fatalf("plain delete: %d %v", code, out)
	}
	if code, out := mut("POST", "/api/hosts", `{"name":"rb8","host":"10.0.0.8","pass":{"kind":"arn","value":"arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/hosts/rb8/pass"}}`); code != 200 {
		t.Fatalf("re-add: %d %v", code, out)
	}

	code, out := mut("DELETE", "/api/hosts/rb8?delete_secret=1", "")
	rep, _ := out["secrets"].(map[string]any)
	if code != 200 || fmt.Sprint(rep["deleted"]) != "[/mrsh/hosts/rb8/pass]" || mem.params["/mrsh/hosts/rb8/pass"] != "" {
		t.Fatalf("delete with secrets: %d %v", code, out)
	}

	// A parameter another entry still uses is kept.
	code, out = mut("DELETE", "/api/hosts/rb7?delete_secret=1", "")
	rep, _ = out["secrets"].(map[string]any)
	if code != 200 || !strings.Contains(fmt.Sprint(rep["kept"]), "still used by credential lab") || mem.params["/mrsh/creds/lab/pass"] != "lab-pw" {
		t.Fatalf("shared: %d %v", code, out)
	}
	if code, out := mut("DELETE", "/api/credentials/lab?delete_secret=1", ""); code != 200 || mem.params["/mrsh/creds/lab/pass"] != "" {
		t.Fatalf("credential delete: %d %v", code, out)
	}
}

func TestRotateRoutes(t *testing.T) {
	f := newFixture(t)
	if w, _ := f.do("POST", "/api/rotate/plan", `{"kind":"credential","name":"lab"}`); w.Code != http.StatusNotImplemented {
		t.Errorf("no rotator: %d", w.Code)
	}

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var applied []string
	f.srv.Rotate = &Rotator{
		Plan: func(_ context.Context, kind, name string) (RotatePlan, error) {
			if name == "ghost" {
				return RotatePlan{}, fmt.Errorf("%w: ghost", config.ErrCredentialNotFound)
			}
			return RotatePlan{Label: kind + " " + name, Hosts: []string{"rt1"}, Store: "ssm", Dest: "/mrsh/creds/lab/pass"}, nil
		},
		Apply: func(ctx context.Context, kind, name string, progress func(RotateResult)) (RotateOutcome, error) {
			switch name {
			case "ghost":
				return RotateOutcome{}, fmt.Errorf("%w: ghost", config.ErrHostNotFound)
			case "slow":
				started <- struct{}{}
				<-release
			}
			if ctx.Done() != nil {
				t.Error("a rotation must not stop when the page goes away")
			}
			applied = append(applied, kind+" "+name)
			if progress != nil {
				progress(RotateResult{Name: "rt1", OK: true, Running: true, Summary: "changing the password"})
			}
			return RotateOutcome{OK: true, Message: "rotated", Results: []RotateResult{{Name: "rt1", OK: true, Summary: "password changed"}}}, nil
		},
	}
	f.h = f.srv.Handler()

	if w, out := f.do("POST", "/api/rotate/plan", `{"kind":"credential","name":"lab"}`); w.Code != 200 || out["dest"] != "/mrsh/creds/lab/pass" || out["store"] != "ssm" {
		t.Errorf("plan: %d %v", w.Code, out)
	}
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/api/rotate/plan", `{"kind":"group","name":"lab"}`, 400},
		{"/api/rotate/plan", `{"kind":"host"}`, 400},
		{"/api/rotate/plan", `{"kind":"credential","name":"ghost"}`, 404},
		{"/api/rotate/apply", `{"kind":"credential","name":"lab"}`, 400},
		{"/api/rotate/apply", `{"kind":"host","name":"ghost","confirm":true}`, 404},
		{"/api/rotate/apply", `{"kind":"host","name":"ghost","confirm":true,"stream":true}`, 404},
	} {
		if w, _ := f.do("POST", tc.path, tc.body); w.Code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.path, tc.body, w.Code, tc.want)
		}
	}
	if len(applied) != 0 {
		t.Fatalf("refused requests rotated: %v", applied)
	}

	// Streamed: progress, then the outcome.
	w, _ := f.do("POST", "/api/rotate/apply", `{"kind":"credential","name":"lab","confirm":true,"stream":true}`)
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || len(lines) != 2 ||
		!strings.Contains(lines[0], `"running":true`) || !strings.HasPrefix(lines[1], `{"outcome":{"ok":true`) {
		t.Errorf("stream: %d %q", w.Code, lines)
	}

	// One rotation at a time.
	done := make(chan int, 1)
	go func() {
		w, _ := f.do("POST", "/api/rotate/apply", `{"kind":"host","name":"slow","confirm":true}`)
		done <- w.Code
	}()
	<-started
	if w, _ := f.do("POST", "/api/rotate/apply", `{"kind":"credential","name":"lab","confirm":true}`); w.Code != http.StatusConflict {
		t.Errorf("concurrent rotation: %d", w.Code)
	}
	close(release)
	if code := <-done; code != 200 || fmt.Sprint(applied) != "[credential lab host slow]" {
		t.Errorf("first rotation: %d %v", code, applied)
	}
}
