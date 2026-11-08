package condition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockfileConditionStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	present := NewLockfile(path)
	present.State = LockfilePresent
	if result := present.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("missing present status = %s, want unsatisfied", result.Status)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := present.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("present status = %s, want satisfied", result.Status)
	}
	absent := NewLockfile(path)
	if result := absent.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("existing absent status = %s, want unsatisfied", result.Status)
	}
}

func TestLockfileDanglingSymlinkCountsAsPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing-target"), path); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	absent := NewLockfile(path)
	if result := absent.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("dangling symlink absent status = %s, want unsatisfied", result.Status)
	}
	present := NewLockfile(path)
	present.State = LockfilePresent
	if result := present.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("dangling symlink present status = %s, want satisfied", result.Status)
	}
}

func TestLockfileConditionOlderThan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cond := NewLockfile(path)
	cond.State = LockfilePresent
	cond.OlderThan = time.Hour
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("fresh lock status = %s, want unsatisfied", result.Status)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("old lock status = %s, err = %v", result.Status, result.Err)
	}
}

func TestLockfileValidationAndDescriptor(t *testing.T) {
	badState := NewLockfile("lock")
	badState.State = "busy"
	if result := badState.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("bad state status = %s, want fatal", result.Status)
	}
	desc := NewLockfile("/tmp/app.lock").Descriptor()
	if desc.Backend != "lockfile" || desc.Target != "/tmp/app.lock" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestLockfileAdditionalBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := NewLockfile("lock").Check(ctx); result.Status != CheckUnsatisfied {
		t.Fatalf("cancelled status = %s, want unsatisfied", result.Status)
	}

	negativeAge := NewLockfile("lock")
	negativeAge.OlderThan = -time.Second
	if result := negativeAge.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("negative older-than status = %s, want fatal", result.Status)
	}
	absent := NewLockfile("lock")
	absent.OlderThan = time.Second
	if result := absent.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("absent older-than status = %s, want fatal", result.Status)
	}
}
