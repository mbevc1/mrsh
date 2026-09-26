package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/spf13/cobra"
)

// maxLambdaOutput caps the stdout returned in the response; the full
// output is in CloudWatch Logs. Lambda responses are limited to 6 MB.
const maxLambdaOutput = 256 << 10

// lambdaEvent is the invocation payload, e.g.
// {"args": ["-g", "routers", "mt", "backup", "--path", "s3://bucket/backups/"]}.
type lambdaEvent struct {
	Args []string `json:"args"`
}

type lambdaResult struct {
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated,omitempty"`
}

func newLambdaCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "lambda",
		Short:  "Serve AWS Lambda invocations (container image entrypoint)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if os.Getenv("AWS_LAMBDA_RUNTIME_API") == "" {
				return errors.New("mrsh lambda runs only inside AWS Lambda (AWS_LAMBDA_RUNTIME_API is not set)")
			}
			h := lambdaHandler{stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr()}
			lambda.StartWithOptions(h.handle, lambda.WithContext(cmd.Context()))
			return nil
		},
	}
}

type lambdaHandler struct {
	stdout, stderr io.Writer
}

// handle runs one mrsh command line per invocation. Stdin is empty, so
// commands that prompt refuse unless given --confirm. The invocation
// context carries the Lambda deadline, which cancels in-flight sessions.
func (h lambdaHandler) handle(ctx context.Context, ev lambdaEvent) (lambdaResult, error) {
	if len(ev.Args) == 0 {
		return lambdaResult{ExitCode: exitUsage}, errors.New(`event needs "args", e.g. {"args": ["run", "-c", "uptime"]}`)
	}
	if c, _, err := newRootCmd().Find(ev.Args); err == nil && (c.Name() == "ui" || c.Name() == "lambda") {
		return lambdaResult{ExitCode: exitUsage}, fmt.Errorf("%q cannot run inside Lambda", c.Name())
	}

	out := &cappedBuffer{limit: maxLambdaOutput}
	root := newRootCmd()
	root.SetOut(io.MultiWriter(h.stdout, out))
	root.SetErr(h.stderr)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(ev.Args)
	err := root.ExecuteContext(ctx)

	res := lambdaResult{ExitCode: exitCode(err), Output: out.String(), Truncated: out.truncated}
	if err != nil {
		return res, &MrshError{Code: res.ExitCode, Err: err}
	}
	return res, nil
}

// MrshError is a failed invocation. Lambda reports its type name as the
// errorType, so failures show up as "MrshError" in logs and metrics.
type MrshError struct {
	Code int
	Err  error
}

func (e *MrshError) Error() string { return fmt.Sprintf("mrsh exited %d: %v", e.Code, e.Err) }
func (e *MrshError) Unwrap() error { return e.Err }

// cappedBuffer keeps the first limit bytes and drops the rest.
type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
