package webui

import (
	"context"
	"fmt"
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

func (m *memSecrets) DeleteSecret(context.Context, string) error { return nil }

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
