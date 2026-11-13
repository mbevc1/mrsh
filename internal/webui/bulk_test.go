package webui

import (
	"fmt"
	"strings"
	"testing"
)

func TestBulkEditHosts(t *testing.T) {
	f := newFixture(t)
	mut := func(body string) (int, map[string]any) {
		t.Helper()
		w, out := f.do("POST", "/api/hosts/bulk-edit", body, header("If-Match", f.version()))
		return w.Code, out
	}

	// Only the fields sent change; the rest of each host is kept.
	if code, out := mut(`{"hosts":["web01","db01"],"changes":{"group":" lab ","port":2222,"pass":{"kind":"env","value":"LAB_PASS"}}}`); code != 200 {
		t.Fatalf("edit: %d %v", code, out)
	}
	web, _ := find(f.config(), "web01")
	db, _ := find(f.config(), "db01")
	if web.Group != "lab" || db.Group != "lab" || web.Port != 2222 || db.Port != 2222 ||
		web.PassEnv != "LAB_PASS" || web.Pass != "" || db.PassARN != "" || web.User != "admin" || web.Host != "10.0.0.1" {
		t.Errorf("after edit: web=%+v db=%+v", web, db)
	}
	// "unset" clears back to inherited; an empty string clears a text field.
	if code, out := mut(`{"hosts":["web01"],"changes":{"user":{"kind":"unset"},"group":""}}`); code != 200 {
		t.Fatalf("clear: %d %v", code, out)
	}
	if web, _ := find(f.config(), "web01"); web.User != "" || web.Group != "" || web.PassEnv != "LAB_PASS" {
		t.Errorf("after clear: %+v", web)
	}

	before, _ := find(f.config(), "db01")
	for body, want := range map[string]string{
		`{"hosts":[],"changes":{"port":1}}`:                                        "no hosts selected",
		`{"hosts":["web01","web01"],"changes":{"port":1}}`:                         "listed twice",
		`{"hosts":["web01"],"changes":{}}`:                                         "nothing to change",
		`{"hosts":["ghost"],"changes":{"port":1}}`:                                 "ghost",
		`{"hosts":["web01","db01"],"changes":{"pass":{"kind":"plain"}}}`:           "enter a literal value",
		`{"hosts":["web01","db01"],"changes":{"pass":{"kind":"ssm","value":"x"}}}`: "credential set",
		`{"hosts":["web01","db01"],"changes":{"credential":"nosuch"}}`:             "nosuch",
		`{"hosts":["web01"],"changes":{"name":"x"}}`:                               "unknown field",
	} {
		if code, out := mut(body); code < 400 || !strings.Contains(fmt.Sprint(out), want) {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if after, _ := find(f.config(), "db01"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a refused edit changed db01: %+v", after)
	}
}

func TestBulkDeleteHosts(t *testing.T) {
	f := newFixture(t)
	mem := &memSecrets{params: map[string]string{}}
	f.srv.Secrets = mem
	f.h = f.srv.Handler()
	mut := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		w, out := f.do(method, path, body, header("If-Match", f.version()))
		return w.Code, out
	}
	// Two hosts sharing one stored password, and one more.
	shared := "arn:aws:ssm:eu-west-1:123456789012:parameter/mrsh/hosts/lab/pass"
	mem.params["/mrsh/hosts/lab/pass"] = "pw"
	for _, n := range []string{"lab1", "lab2"} {
		if code, out := mut("POST", "/api/hosts", `{"name":"`+n+`","host":"10.0.1.1","pass":{"kind":"arn","value":"`+shared+`"}}`); code != 200 {
			t.Fatalf("add %s: %d %v", n, code, out)
		}
	}

	// One of them alone keeps the parameter for the other.
	if code, out := mut("POST", "/api/hosts/bulk-delete", `{"hosts":["lab1"],"delete_secret":true}`); code != 200 ||
		!strings.Contains(fmt.Sprint(out["secrets"]), "still used by host lab2") || mem.params["/mrsh/hosts/lab/pass"] == "" {
		t.Fatalf("one: %d %v", code, out)
	}
	if code, out := mut("POST", "/api/hosts", `{"name":"lab1","host":"10.0.1.1","pass":{"kind":"arn","value":"`+shared+`"}}`); code != 200 {
		t.Fatalf("re-add: %d %v", code, out)
	}
	// Together, it goes with them; without the flag nothing in SSM changes.
	if code, out := mut("POST", "/api/hosts/bulk-delete", `{"hosts":["lab1","lab2"],"delete_secret":true}`); code != 200 ||
		fmt.Sprint(out["secrets"].(map[string]any)["deleted"]) != "[/mrsh/hosts/lab/pass]" || mem.params["/mrsh/hosts/lab/pass"] != "" {
		t.Fatalf("both: %d %v", code, out)
	}
	if code, out := mut("POST", "/api/hosts/bulk-delete", `{"hosts":["web01","db01"]}`); code != 200 || out["secrets"] != nil {
		t.Fatalf("plain: %d %v", code, out)
	}
	if hosts := f.config().Hosts; len(hosts) != 0 {
		t.Errorf("left: %+v", hosts)
	}
	if code, _ := mut("POST", "/api/hosts/bulk-delete", `{"hosts":["ghost"]}`); code != 404 {
		t.Errorf("ghost: %d", code)
	}
}
