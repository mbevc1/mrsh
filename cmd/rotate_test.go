package cmd

import (
	"fmt"
	"os"
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
	if err != nil || !strings.Contains(out, "rotated the password of credential routers on 2 device(s); stored in SSM parameter /mrsh/creds/routers/pass") {
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
	if exitCode(err) != exitHostFailed || !strings.Contains(err.Error(), "every device is back on the old password and SSM parameter /mrsh/creds/routers/pass is unchanged") {
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
	if exitCode(err) != exitHostFailed || !strings.Contains(err.Error(), "rt1 still has the new password; it is kept in SSM as /mrsh/creds/routers/pass-next") {
		t.Fatalf("err = %v", err)
	}
	staged := fake.Param("/mrsh/creds/routers/pass-next")
	if staged == nil || staged.Value != rt1.password() || fake.Param("/mrsh/creds/routers/pass").Value != "old" {
		t.Errorf("staging=%+v rt1=%q", staged, rt1.password())
	}
}

func TestRotateKeepsTheStoreAndHostRules(t *testing.T) {
	fake := ssmtest.Start(t)
	rt1, envRt, box := startPwRouter(t, "old"), startPwRouter(t, "envpw"), sshtest.Start(t, sshtest.Options{User: "admin", Password: "boxpw"})
	t.Setenv("RT_ENV_PASS", "envpw")
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass: old}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("envrt", envRt, ", user: admin, pass_env: RT_ENV_PASS")+
		fmt.Sprintf("  - {name: box, host: %s, port: %d, user: admin, pass: boxpw}\n", box.Host, box.Port)+
		"  - {name: sm, host: 10.0.0.9, user: admin, pass_arn: arn:aws:secretsmanager:eu-west-1:123456789012:secret:rt-AbCdEf}\n")

	// Refusals: moving the password while rotating, other changes alongside,
	// a host on a set's password, and Secrets Manager.
	for args, want := range map[string]string{
		"creds set --name routers --rotate-pass --pass-ssm -y": "run --pass-ssm on its own first",
		"creds set --name routers --rotate-pass -u x -y":       "changes only the password",
		"hosts update --name rt1 --rotate-pass -y":             "credentials set --name routers --rotate-pass",
		"hosts update --name sm --rotate-pass -y":              "is not an SSM parameter",
	} {
		if _, _, err := execIn(t, "", append([]string{"-q", "-f", cfg}, strings.Fields(args)...)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}
	// Dry run touches nothing.
	if out, _, err := execIn(t, "", "-q", "-f", cfg, "--dry-run", "creds", "set", "--name", "routers", "--rotate-pass"); err != nil || !strings.Contains(out, "kept in the config") || rt1.password() != "old" {
		t.Fatalf("dry run: %v\n%s", err, out)
	}

	// A literal stays a literal, with the new value; off a terminal it is not printed.
	out, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y")
	set := loadCfg(t, cfg).Credentials["routers"]
	if err != nil || set.Pass != rt1.password() || set.Pass == "old" || set.PassARN != "" || !strings.Contains(out, "stored in the config") || strings.Contains(out, set.Pass) {
		t.Fatalf("literal: %v set=%+v rt1=%q\n%s", err, set, rt1.password(), out)
	}

	// An environment variable stays one: the new password is printed, since
	// nothing else keeps it, and this process uses it from now on.
	out, _, err = execIn(t, "", "-q", "-f", cfg, "hosts", "update", "--name", "envrt", "--rotate-pass", "-y")
	h, _ := findHost(loadCfg(t, cfg), "envrt")
	if err != nil || envRt.password() == "envpw" || !strings.Contains(out, "new password for host envrt (shown once): "+envRt.password()) ||
		!strings.Contains(out, "set RT_ENV_PASS to the new password") || h.PassEnv != "RT_ENV_PASS" || os.Getenv("RT_ENV_PASS") != envRt.password() {
		t.Fatalf("env: %v host=%+v\n%s", err, h, out)
	}

	// Only RouterOS passwords rotate; a failure changes nothing.
	if _, _, err := execIn(t, "", "-q", "-f", cfg, "hosts", "update", "--name", "box", "--rotate-pass", "-y"); exitCode(err) != exitHostFailed {
		t.Fatalf("linux host: %v", err)
	}
	if h, _ := findHost(loadCfg(t, cfg), "box"); h.Pass != "boxpw" || len(fake.Calls()) != 0 {
		t.Errorf("a failed rotation changed something: %+v calls=%v", h, fake.Calls())
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
		!strings.Contains(string(body), `"hosts":["rt1","rt2"],"store":"ssm","dest":"/mrsh/creds/routers/pass"`) {
		t.Fatalf("plan: %d %s", code, body)
	}
	if code, body := post("/api/rotate/plan", `{"kind":"host","name":"rt1"}`); code != 422 || !strings.Contains(string(body), "uses the password of credential routers") {
		t.Errorf("inherited: %d %s", code, body)
	}
	if code, body := post("/api/rotate/plan", `{"kind":"host","name":"lit"}`); code != 200 || !strings.Contains(string(body), `"store":"literal"`) {
		t.Errorf("literal: %d %s", code, body)
	}

	// Streamed: each device's steps, then the outcome, which alone carries
	// the new password for the page to show once.
	code, body := post("/api/rotate/apply", `{"kind":"credential","name":"routers","confirm":true,"stream":true}`)
	newPass := fake.Param("/mrsh/creds/routers/pass").Value
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	last := lines[len(lines)-1]
	if code != 200 || newPass == "old" || rt1.password() != newPass || rt2.password() != newPass ||
		!strings.Contains(string(body), `"summary":"changing the password"`) ||
		!strings.HasPrefix(last, `{"outcome":{"ok":true,"message":"rotated the password of credential routers on 2 device(s)`) {
		t.Fatalf("apply: %d\n%s", code, body)
	}
	if strings.Contains(strings.Join(lines[:len(lines)-1], "\n"), newPass) || !strings.Contains(last, `"password":"`+newPass+`"`) || strings.Contains(string(body), `"old"`) {
		t.Errorf("the new password should be in the outcome only:\n%s", body)
	}

	// A host's own SSM password rotates in place under its own name.
	if code, body := post("/api/rotate/apply", `{"kind":"host","name":"own","confirm":true}`); code != 200 || !strings.Contains(string(body), `"ok":true`) ||
		own.password() != fake.Param("/ops/own-pass").Value || own.password() == "mine" {
		t.Errorf("own: %d %s", code, body)
	}

	// A literal stays a literal, with the new value.
	if code, body := post("/api/rotate/apply", `{"kind":"host","name":"lit","confirm":true}`); code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("literal: %d %s", code, body)
	}
	h, _ := findHost(loadCfg(t, cfg), "lit")
	if h.Pass != lit.password() || h.Pass == "lit" || h.PassARN != "" || fake.Param("/mrsh/hosts/lit/pass") != nil {
		t.Errorf("after rotation: %+v lit=%q", h, lit.password())
	}

	// A failure rolls back and says so; the reply is still 200 with ok false.
	rt2.allow(0)
	before := rt1.password()
	code, body = post("/api/rotate/apply", `{"kind":"credential","name":"routers","confirm":true}`)
	if code != 200 || !strings.Contains(string(body), `"ok":false`) || !strings.Contains(string(body), "every device is back on the old password") ||
		!strings.Contains(string(body), "rolled back: another device failed") || strings.Contains(string(body), `"password"`) ||
		rt1.password() != before || fake.Param("/mrsh/creds/routers/pass").Value != before {
		t.Errorf("rollback: %d %s", code, body)
	}
}

func TestRotateLiteralShowsPasswordWhenStuck(t *testing.T) {
	rt1, rt2 := startPwRouter(t, "old"), startPwRouter(t, "old")
	rt1.allow(1) // takes the new password, then refuses to go back
	rt2.allow(0)
	cfg := writeConfig(t, "defaults: {host_key_policy: insecure}\ncredentials:\n  routers: {user: admin, pass: old}\nhosts:\n"+
		hostLine("rt1", rt1, ", credential: routers")+hostLine("rt2", rt2, ", credential: routers"))

	out, _, err := execIn(t, "", "-q", "-f", cfg, "creds", "set", "--name", "routers", "--rotate-pass", "-y")
	if exitCode(err) != exitHostFailed || !strings.Contains(err.Error(), "rt1 still has the new password, shown below; the config still holds the old one") ||
		!strings.Contains(out, "new password for credential routers (shown once): "+rt1.password()) || rt1.password() == "old" {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if loadCfg(t, cfg).Credentials["routers"].Pass != "old" {
		t.Error("the config changed after a failed rotation")
	}
}
