package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func runLambda(t *testing.T, args ...string) (lambdaResult, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	h := lambdaHandler{stdout: &stdout, stderr: &stderr}
	res, err := h.handle(context.Background(), lambdaEvent{Args: args})
	return res, stdout.String(), err
}

func TestLambdaHandlerRunsCommand(t *testing.T) {
	res, logged, err := runLambda(t, "version", "-o", "json")
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, `"name":"mrsh"`) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if logged != res.Output {
		t.Errorf("stdout not mirrored to the log stream: %q vs %q", logged, res.Output)
	}
}

func TestLambdaHandlerFailures(t *testing.T) {
	cfg := writeConfig(t, "hosts:\n  - {name: a, host: 192.0.2.1}\n")
	tests := []struct {
		args []string
		code int
		want string
	}{
		{nil, exitUsage, `event needs "args"`},
		{[]string{"ui"}, exitUsage, `"ui" cannot run inside Lambda`},
		{[]string{"-f", cfg, "ui", "--no-open"}, exitUsage, `"ui" cannot run inside Lambda`},
		{[]string{"lambda"}, exitUsage, `"lambda" cannot run inside Lambda`},
		{[]string{"-f", cfg, "run"}, exitUsage, "mrsh exited 2: nothing to run"},
		// Prompts see empty stdin and refuse without --confirm.
		{[]string{"-f", cfg, "mt", "reboot"}, exitUsage, "aborted"},
	}
	for _, tt := range tests {
		res, _, err := runLambda(t, tt.args...)
		if err == nil || res.ExitCode != tt.code || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: res=%+v err=%v, want exit %d and %q", tt.args, res, err, tt.code, tt.want)
		}
	}
}

func TestLambdaHandlerHostFailureIsError(t *testing.T) {
	cfg := serverConfig(t)
	res, _, err := runLambda(t, "-f", cfg, "-H", "badpw", "run", "-c", "true")
	if err == nil || res.ExitCode != exitHostFailed || !strings.Contains(res.Output, "badpw") {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestLambdaCommandRefusesOutsideLambda(t *testing.T) {
	t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
	if _, err := execRoot(t, "lambda"); err == nil || !strings.Contains(err.Error(), "only inside AWS Lambda") {
		t.Errorf("err = %v", err)
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{limit: 5}
	n, _ := b.Write([]byte("abc"))
	m, _ := b.Write([]byte("defgh"))
	if n != 3 || m != 5 || b.String() != "abcde" || !b.truncated {
		t.Errorf("n=%d m=%d s=%q truncated=%v", n, m, b.String(), b.truncated)
	}
}
