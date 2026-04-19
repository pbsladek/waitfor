package condition

import (
	"strings"
	"testing"

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

func TestExecConditionCommandNotFoundIsFatal(t *testing.T) {
	cond := NewExec([]string{"/definitely/missing/waitfor-command"})

	result := cond.Check(t.Context())
	if result.Err == nil {
		t.Fatal("Err = nil, want command error")
	}
	if result.Status != CheckFatal {
		t.Fatalf("Status = %s, want %s", result.Status, CheckFatal)
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
