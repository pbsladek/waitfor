package condition

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandSecurityHelpers(t *testing.T) {
	if err := rejectOptionLike("value", "-bad"); err == nil {
		t.Fatal("option-like value succeeded")
	}
	if err := classifyLimitedCommandError(errors.New("exit status 1"), commandOutput{}, context.Canceled); err != context.Canceled {
		t.Fatalf("context error = %v, want context.Canceled", err)
	}
	if !errors.Is(classifyLimitedCommandError(exec.ErrNotFound, commandOutput{}, nil), exec.ErrNotFound) {
		t.Fatal("exec.ErrNotFound was not preserved")
	}
	err := classifyLimitedCommandError(errors.New("exit status 1"), commandOutput{stderr: []byte("denied")}, nil)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want command output detail", err)
	}
	truncated := classifyLimitedCommandError(errors.New("exit status 1"), commandOutput{stdout: []byte("x"), truncated: true}, nil)
	if truncated == nil || !strings.Contains(truncated.Error(), "truncated") {
		t.Fatalf("error = %v, want truncation marker", truncated)
	}
}
