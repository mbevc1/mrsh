package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/mbevc1/mrsh/internal/mt"
	"github.com/mbevc1/mrsh/internal/sshtest"
	"github.com/mbevc1/mrsh/internal/ssmtest"
)

// pwRouter is a RouterOS device whose login password /password changes.
type pwRouter struct {
	srv *sshtest.Server

	mu      sync.Mutex
	pass    string
	allowed int // password changes it accepts; -1 is unlimited
}

var passwordArgs = regexp.MustCompile(`^/password old-password="([^"]*)" new-password="([^"]*)" confirm-new-password="([^"]*)"$`)

func startPwRouter(t *testing.T, pass string) *pwRouter {
	t.Helper()
	r := &pwRouter{pass: pass, allowed: -1}
	r.srv = sshtest.Start(t, sshtest.Options{User: "admin", PasswordFunc: r.login, Exec: r.exec})
	return r
}

func (r *pwRouter) login(p string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return p == r.pass
}

func (r *pwRouter) password() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pass
}

func (r *pwRouter) allow(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowed = n
}

func (r *pwRouter) exec(cmd string) sshtest.Reply {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case cmd == mt.RouterboardCmd:
		return sshtest.Reply{Stdout: rbSample}
	case cmd == mt.VersionScript:
		return sshtest.Reply{Stdout: "ros=7.14 (stable)\npkg=routeros 7.14\n"}
	case strings.HasPrefix(cmd, "/password "):
		m := passwordArgs.FindStringSubmatch(cmd)
		switch {
		case m == nil || m[2] != m[3]:
			return sshtest.Reply{Stdout: "syntax error\n"}
		case r.allowed == 0:
			return sshtest.Reply{Stdout: "failure: password change not allowed\n"}
		case m[1] != r.pass:
			return sshtest.Reply{Stdout: "failure: old password is not valid\n"}
		}
		r.pass = m[2]
		if r.allowed > 0 {
			r.allowed--
		}
		return sshtest.Reply{}
	}
	return sshtest.Reply{Stdout: "bad command name\n"}
}

func hostLine(name string, r *pwRouter, extra string) string {
	return fmt.Sprintf("  - {name: %s, host: %s, port: %d%s}\n", name, r.srv.Host, r.srv.Port, extra)
}

func TestRotateCredentialInPlace(t *testing.T) {
	fake := ssmtest.Start(t)
	arn := fake.Put("/mrsh/creds/routers/pass", "old")
	rt1, rt2, own := startPwRouter(t, "old"), startPwRouter(t, "old"), startPwRouter(t, "mine")
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass_arn: "+arn+"}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("rt2", rt2, ", credential: routers")+hostLine("own", own, ", user: admin, pass: mine"))
	before := readFile(t, cfg)

	out, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y")
	if err != nil || !strings.Contains(out, "rotated the password of credential routers on 2 device(s); stored in SSM as /mrsh/creds/routers/pass") {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	newPass := fake.Param("/mrsh/creds/routers/pass").Value
	if newPass == "old" || len(newPass) != 24 || rt1.password() != newPass || rt2.password() != newPass {
		t.Fatalf("passwords: ssm=%q rt1=%q rt2=%q", newPass, rt1.password(), rt2.password())
	}
	if own.password() != "mine" || fake.Param("/mrsh/creds/routers/pass-next") != nil || readFile(t, cfg) != before {
		t.Errorf("side effects: own=%q staging=%v config changed=%v", own.password(), fake.Param("/mrsh/creds/routers/pass-next"), readFile(t, cfg) != before)
	}
	if strings.Contains(out, newPass) {
		t.Error("the new password was printed")
	}
}

func TestRotateRollsBackOnFailure(t *testing.T) {
	fake := ssmtest.Start(t)
	arn := fake.Put("/mrsh/creds/routers/pass", "old")
	rt1, rt2 := startPwRouter(t, "old"), startPwRouter(t, "old")
	rt2.allow(0) // refuses the change
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass_arn: "+arn+"}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("rt2", rt2, ", credential: routers"))

	out, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y")
	if exitCode(err) != exitHostFailed || !strings.Contains(err.Error(), "every device is back on the old password and SSM is unchanged") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if rt1.password() != "old" || rt2.password() != "old" || fake.Param("/mrsh/creds/routers/pass").Value != "old" || fake.Param("/mrsh/creds/routers/pass-next") != nil {
		t.Errorf("after rollback: rt1=%q rt2=%q ssm=%q staging=%v", rt1.password(), rt2.password(), fake.Param("/mrsh/creds/routers/pass").Value, fake.Param("/mrsh/creds/routers/pass-next"))
	}
	if !strings.Contains(out, "rolled back: another device failed") || !strings.Contains(out, "password change not allowed") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRotateKeepsNewPasswordWhenRollbackFails(t *testing.T) {
	fake := ssmtest.Start(t)
	arn := fake.Put("/mrsh/creds/routers/pass", "old")
	rt1, rt2 := startPwRouter(t, "old"), startPwRouter(t, "old")
	rt1.allow(1) // takes the new password, then refuses to go back
	rt2.allow(0)
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass_arn: "+arn+"}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("rt2", rt2, ", credential: routers"))

	_, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y")
	if exitCode(err) != exitHostFailed || !strings.Contains(err.Error(), "rt1 still have the new password; it is kept in SSM as /mrsh/creds/routers/pass-next") {
		t.Fatalf("err = %v", err)
	}
	staged := fake.Param("/mrsh/creds/routers/pass-next")
	if staged == nil || staged.Value != rt1.password() || fake.Param("/mrsh/creds/routers/pass").Value != "old" {
		t.Errorf("staging=%+v rt1=%q", staged, rt1.password())
	}
}

func TestRotateIntoSSMAndHostRules(t *testing.T) {
	fake := ssmtest.Start(t)
	rt1, box := startPwRouter(t, "old"), sshtest.Start(t, sshtest.Options{User: "admin", Password: "boxpw"})
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass: old}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+fmt.Sprintf("  - {name: box, host: %s, port: %d, user: admin, pass: boxpw}\n", box.Host, box.Port))

	// A literal password must be given an SSM home.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y"); err == nil || !strings.Contains(err.Error(), "add --pass-ssm") {
		t.Fatalf("literal source: %v", err)
	}
	// A host on a set's password points at the set.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "hosts", "update", "--name", "rt1", "--rotate-pass", "-y"); err == nil || !strings.Contains(err.Error(), "credentials set --name routers --rotate-pass") {
		t.Fatalf("inherited: %v", err)
	}
	// Dry run touches nothing.
	if out, _, err := execIn(t, "", "-q", "-f", cfg, "--dry-run", "creds", "set", "--name", "routers", "--rotate-pass", "--pass-ssm"); err != nil || !strings.Contains(out, "rt1") || len(fake.Calls()) != 0 || rt1.password() != "old" {
		t.Fatalf("dry run: %v calls=%v\n%s", err, fake.Calls(), out)
	}
	// Other changes alongside are refused.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-u", "x"); err == nil || !strings.Contains(err.Error(), "changes only the password") {
		t.Fatalf("mixed: %v", err)
	}

	// Moving into SSM: the config switches from the literal to the new parameter.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "--pass-ssm", "-y"); err != nil {
		t.Fatal(err)
	}
	set := loadCfg(t, cfg).Credentials["routers"]
	if set.Pass != "" || set.PassARN != fake.ARN("/mrsh/creds/routers/pass") || fake.Param("/mrsh/creds/routers/pass").Value != rt1.password() {
		t.Errorf("after move: set=%+v rt1=%q", set, rt1.password())
	}

	// Only RouterOS passwords rotate; a failure here leaves no new parameter.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "hosts", "update", "--name", "box", "--rotate-pass", "--pass-ssm", "-y"); exitCode(err) != exitHostFailed {
		t.Fatalf("linux host: %v", err)
	}
	if fake.Param("/mrsh/hosts/box/pass") != nil || loadCfg(t, cfg).Hosts[1].Pass != "boxpw" {
		t.Error("a failed rotation left a parameter or changed the config")
	}
}

func TestUIRotation(t *testing.T) {
	fake := ssmtest.Start(t)
	arn := fake.Put("/mrsh/creds/routers/pass", "old")
	rt1, rt2, own, lit := startPwRouter(t, "old"), startPwRouter(t, "old"), startPwRouter(t, "mine"), startPwRouter(t, "lit")
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass_arn: "+arn+"}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("rt2", rt2, ", credential: routers")+
		hostLine("own", own, ", user: admin, pass_arn: "+fake.Put("/ops/own-pass", "mine"))+hostLine("lit", lit, ", user: admin, pass: lit"))
	post := startUI(t, cfg)

	// The plan names the devices and where the password goes.
	if code, body := post("/api/rotate/plan", `{"kind":"credential","name":"routers"}`); code != 200 ||
		!strings.Contains(string(body), `"hosts":["rt1","rt2"],"dest":"/mrsh/creds/routers/pass","in_place":true`) {
		t.Fatalf("plan: %d %s", code, body)
	}
	if code, body := post("/api/rotate/plan", `{"kind":"host","name":"rt1"}`); code != 422 || !strings.Contains(string(body), "uses the password of credential routers") {
		t.Errorf("inherited: %d %s", code, body)
	}
	if code, body := post("/api/rotate/plan", `{"kind":"host","name":"lit"}`); code != 200 || !strings.Contains(string(body), `"dest":"/mrsh/hosts/lit/pass","in_place":false`) {
		t.Errorf("literal: %d %s", code, body)
	}

	// Streamed: each device's steps, then the outcome; nothing secret.
	code, body := post("/api/rotate/apply", `{"kind":"credential","name":"routers","confirm":true,"stream":true}`)
	newPass := fake.Param("/mrsh/creds/routers/pass").Value
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	last := lines[len(lines)-1]
	if code != 200 || newPass == "old" || rt1.password() != newPass || rt2.password() != newPass ||
		!strings.Contains(string(body), `"summary":"changing the password"`) ||
		!strings.HasPrefix(last, `{"outcome":{"ok":true,"message":"rotated the password of credential routers on 2 device(s)`) {
		t.Fatalf("apply: %d\n%s", code, body)
	}
	if strings.Contains(string(body), newPass) || strings.Contains(string(body), `"old"`) {
		t.Error("a password reached the page")
	}

	// A host's own SSM password rotates in place under its own name.
	if code, body := post("/api/rotate/apply", `{"kind":"host","name":"own","confirm":true}`); code != 200 || !strings.Contains(string(body), `"ok":true`) ||
		own.password() != fake.Param("/ops/own-pass").Value || own.password() == "mine" {
		t.Errorf("own: %d %s", code, body)
	}

	// A literal password moves to SSM and the config follows.
	if code, body := post("/api/rotate/apply", `{"kind":"host","name":"lit","confirm":true}`); code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("literal: %d %s", code, body)
	}
	h, _ := findHost(loadCfg(t, cfg), "lit")
	if h.Pass != "" || h.PassARN != fake.ARN("/mrsh/hosts/lit/pass") || fake.Param("/mrsh/hosts/lit/pass").Value != lit.password() {
		t.Errorf("after move: %+v lit=%q", h, lit.password())
	}

	// A failure rolls back and says so; the reply is still 200 with ok false.
	rt2.allow(0)
	before := rt1.password()
	code, body = post("/api/rotate/apply", `{"kind":"credential","name":"routers","confirm":true}`)
	if code != 200 || !strings.Contains(string(body), `"ok":false`) || !strings.Contains(string(body), "every device is back on the old password") ||
		!strings.Contains(string(body), "rolled back: another device failed") || rt1.password() != before || fake.Param("/mrsh/creds/routers/pass").Value != before {
		t.Errorf("rollback: %d %s", code, body)
	}
}
