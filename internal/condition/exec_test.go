package condition

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/expr"
)

func TestExecConditionSatisfied(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "printf '{\"ready\":true}\\n'"})
	cond.OutputJSONExpr = expr.MustCompile(".ready == true")

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestExecConditionExitCodeMismatch(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "exit 7"})

	result := cond.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("Satisfied = true, want false")
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want exit code error")
	}
}

func TestExecConditionExpectedExitCode(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "exit 7"})
	cond.ExpectedExitCode = 7

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v", result.Err)
	}
}

func TestExecConditionNegativeExpectedExitCodeFatal(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "exit 0"})
	cond.ExpectedExitCode = -1

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestClassifyRunErrorContextCancellationBeatsExitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("cmd.Run() error = nil, want cancellation error")
	}
	_, result := classifyRunError(err, ctx.Err())
	if result == nil {
		t.Fatal("classifyRunError result = nil, want cancellation result")
	}
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestExecConditionDefaultOutputLimit(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "printf ok"})
	if cond.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("MaxOutputBytes = %d, want %d", cond.MaxOutputBytes, DefaultMaxOutputBytes)
	}
}

func TestExecConditionCwdEnvAndOutputLimit(t *testing.T) {
	dir := t.TempDir()
	cond := NewExec([]string{"/bin/sh", "-c", "printf '%s:%s:abcdef' \"$PWD\" \"$WAITFOR_TEST\""})
	cond.Cwd = dir
	cond.Env = []string{"WAITFOR_TEST=yes"}
	cond.OutputContains = ":yes:abc"
	cond.MaxOutputBytes = int64(len(dir) + len(":yes:abc"))

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v, detail = %q", result.Err, result.Detail)
	}
}

func TestExecConditionInvalidJSONOutputUnsatisfied(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "printf 'warming up'"})
	cond.OutputJSONExpr = expr.MustCompile(".ready == true")

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "parse json") {
		t.Fatalf("err = %v, want parse json error", result.Err)
	}
}

func TestExecConditionCommandNotFoundIsFatal(t *testing.T) {
	cond := NewExec([]string{"/definitely/missing/waitfor-command"})

	result := cond.Check(t.Context())
	if result.Err == nil {
		t.Fatal("Err = nil, want command error")
	}
	if result.Status != CheckFatal {
		t.Fatalf("Status = %s, want %s", result.Status, CheckFatal)
	}
	if strings.Contains(result.Err.Error(), "/definitely/missing/waitfor-command") {
		t.Fatalf("Err = %q leaked command path", result.Err)
	}
}

func TestExecDescriptor(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "exit 0"})
	d := cond.Descriptor()
	if d.Backend != "exec" {
		t.Fatalf("Backend = %q, want exec", d.Backend)
	}
	if !strings.Contains(d.Target, "/bin/sh") {
		t.Fatalf("Target = %q, want to contain /bin/sh", d.Target)
	}
	if strings.Contains(d.Target, "exit 0") {
		t.Fatalf("Target = %q leaked command args", d.Target)
	}
}

func TestExecDescriptorRedactsAllArgs(t *testing.T) {
	cond := NewExec([]string{
		"deploy",
		"--token", "secret-token",
		"--password=secret-password",
		"Authorization: Bearer abc",
	})
	d := cond.Descriptor()
	for _, leaked := range []string{"secret-token", "secret-password", "Bearer abc"} {
		if strings.Contains(d.Target, leaked) {
			t.Fatalf("Target = %q leaked %q", d.Target, leaked)
		}
	}
	if d.Target != "deploy [args redacted]" {
		t.Fatalf("Target = %q, want executable with args redacted", d.Target)
	}
}

func TestExecOutputContainsDetailDoesNotExposeSecret(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", "printf secret-token"})
	cond.OutputContains = "secret-token"
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, err = %v", result.Status, result.Err)
	}
	if strings.Contains(result.Detail, "secret-token") {
		t.Fatalf("Detail = %q leaked secret", result.Detail)
	}
}

func TestExecJSONPathErrorDoesNotExposeExpressionValue(t *testing.T) {
	cond := NewExec([]string{"/bin/sh", "-c", `printf '{"token":"actual"}'`})
	cond.OutputJSONExpr = expr.MustCompile(".token == expected-secret")
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("Status = %s, want %s", result.Status, CheckUnsatisfied)
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want jsonpath unsatisfied error")
	}
	if strings.Contains(result.Err.Error(), "expected-secret") || strings.Contains(result.Detail, "expected-secret") {
		t.Fatalf("jsonpath output leaked expected value: detail=%q err=%q", result.Detail, result.Err)
	}
}

func TestLimitedBufferTruncation(t *testing.T) {
	var b limitedBuffer
	b.limit = 5

	n, err := b.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("hello world") {
		t.Fatalf("Write() n = %d, want %d", n, len("hello world"))
	}
	if !b.truncated {
		t.Fatal("truncated = false, want true")
	}
	if got := b.String(); got != "hello" {
		t.Fatalf("buffer = %q, want %q", got, "hello")
	}
}

func TestLimitedBufferFullThenDiscards(t *testing.T) {
	var b limitedBuffer
	b.limit = 3
	_, _ = b.Write([]byte("abc"))
	// Buffer is now full; subsequent writes should be discarded
	n, err := b.Write([]byte("xyz"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len("xyz") {
		t.Fatalf("Write() n = %d, want %d", n, len("xyz"))
	}
	if got := b.String(); got != "abc" {
		t.Fatalf("buffer after overflow = %q, want abc", got)
	}
}

func TestLimitedBufferNoLimit(t *testing.T) {
	var b limitedBuffer
	// limit = 0 means unlimited
	_, _ = b.Write([]byte("unlimited data"))
	if b.String() != "unlimited data" {
		t.Fatalf("buffer = %q, want unlimited data", b.String())
	}
	if b.truncated {
		t.Fatal("truncated = true for unlimited buffer")
	}
}
