package condition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/pbsladek/wait-for/internal/expr"
)

const DefaultMaxOutputBytes int64 = 1024 * 1024

type ExecCondition struct {
	Command          []string
	ExpectedExitCode int
	OutputContains   string
	OutputJSONExpr   *expr.Expression // pre-compiled; use OutputJSONExpr.String() for display
	Cwd              string
	Env              []string
	MaxOutputBytes   int64
}

func NewExec(command []string) *ExecCondition {
	return &ExecCondition{Command: command, MaxOutputBytes: DefaultMaxOutputBytes}
}

func (c *ExecCondition) Descriptor() Descriptor {
	target := commandTarget(c.Command)
	return Descriptor{Backend: "exec", Target: target}
}

func (c *ExecCondition) Check(ctx context.Context) Result {
	if len(c.Command) == 0 || c.Command[0] == "" {
		return Fatal(fmt.Errorf("exec command is required"))
	}
	if c.ExpectedExitCode < 0 {
		return Fatal(fmt.Errorf("exec exit code cannot be negative"))
	}

	cmd := exec.CommandContext(ctx, c.Command[0], c.Command[1:]...) // #nosec G204 -- exec backend exists to run the caller-supplied command.
	prepareExecCommand(cmd)
	cmd.Dir = c.Cwd
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	var output limitedBuffer
	output.limit = c.MaxOutputBytes
	cmd.Stdout = io.Writer(&output)
	cmd.Stderr = io.Writer(&output)

	exitCode, earlyResult := classifyRunError(cmd.Run(), ctx.Err())
	if earlyResult != nil {
		return *earlyResult
	}

	if exitCode != c.ExpectedExitCode {
		detail := fmt.Sprintf("exit code %d, expected %d", exitCode, c.ExpectedExitCode)
		return Unsatisfied(detail, errors.New(detail))
	}

	return checkExecOutput(output.Bytes(), output.truncated, exitCode, c)
}

// classifyRunError maps the error from cmd.Run() to either:
//   - (exitCode, nil): process exited with non-zero; caller checks exit code
//   - (0, &Result): a fatal or context-cancelled result; caller should return it
func classifyRunError(runErr, ctxErr error) (int, *Result) {
	if runErr == nil {
		return 0, nil
	}
	if ctxErr != nil {
		r := Unsatisfied("", ctxErr)
		return 0, &r
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	r := Fatal(errors.New("exec command failed to start"))
	return 0, &r
}

// checkExecOutput evaluates the captured output against any configured matchers.
func checkExecOutput(out []byte, truncated bool, exitCode int, c *ExecCondition) Result {
	details := []string{fmt.Sprintf("exit code %d", exitCode)}
	if truncated {
		details = append(details, fmt.Sprintf("output truncated to %d bytes", c.MaxOutputBytes))
	}
	if c.OutputContains != "" {
		if !bytes.Contains(out, []byte(c.OutputContains)) {
			return Unsatisfied("output substring not found", fmt.Errorf("output does not contain required substring"))
		}
		details = append(details, "output contains required substring")
	}
	if c.OutputJSONExpr != nil {
		ok, detail, err := c.OutputJSONExpr.EvaluateJSON(out)
		if err != nil {
			return Unsatisfied("jsonpath evaluation failed", err)
		}
		if !ok {
			return Unsatisfied(detail, fmt.Errorf("jsonpath condition not satisfied"))
		}
		details = append(details, detail)
	}
	return Satisfied(strings.Join(details, ", "))
}

func commandTarget(command []string) string {
	if len(command) == 0 {
		return ""
	}
	if len(command) == 1 {
		return command[0]
	}
	return command[0] + " [args redacted]"
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.Buffer.Write(p)
	}
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		b.truncated = true
		_, _ = b.Buffer.Write(p[:remaining])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
