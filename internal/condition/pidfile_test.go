package condition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPIDFileConditionRunningSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pid")
	if err := os.WriteFile(path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cond := NewPIDFile(path)
	cond.PIDExists = func(_ context.Context, pid int) (bool, error) {
		return pid == 42, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestPIDFileConditionStoppedWhenAbsent(t *testing.T) {
	cond := NewPIDFile(filepath.Join(t.TempDir(), "missing.pid"))
	cond.State = ProcessStopped

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, want satisfied", result.Status)
	}
}

func TestPIDFileRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pid")
	if err := os.WriteFile(path, bytes.Repeat([]byte("1"), maxPIDFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result := NewPIDFile(path).Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestPIDFileUnsatisfiedBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pid")
	if err := os.WriteFile(path, []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := NewPIDFile(path)
	stale.PIDExists = func(context.Context, int) (bool, error) {
		return false, nil
	}
	if result := stale.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("stale pid status = %s, want unsatisfied", result.Status)
	}
	probeErr := NewPIDFile(path)
	probeErr.PIDExists = func(context.Context, int) (bool, error) {
		return false, errors.New("probe failed")
	}
	if result := probeErr.Check(t.Context()); result.Status != CheckUnsatisfied || result.Err == nil {
		t.Fatalf("probe error result = %+v, want unsatisfied with error", result)
	}
	if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := NewPIDFile(path).Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("invalid pid status = %s, want unsatisfied", result.Status)
	}
}

func TestPIDFileValidationAndDescriptor(t *testing.T) {
	if result := NewPIDFile("").Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing path status = %s, want fatal", result.Status)
	}
	badState := NewPIDFile("pid")
	badState.State = "waiting"
	if result := badState.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("bad state status = %s, want fatal", result.Status)
	}
	desc := NewPIDFile("/tmp/app.pid").Descriptor()
	if desc.Backend != "pidfile" || desc.Target != "/tmp/app.pid" {
		t.Fatalf("descriptor = %+v", desc)
	}
}
