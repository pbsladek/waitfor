package condition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionConditionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	// #nosec G302 -- group-readable mode is the behavior under test.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	cond := NewPermission(path)
	cond.Mode = 0o640
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	cond.Mode = 0o600
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("mismatch status = %s, want unsatisfied", result.Status)
	}
}

func TestPermissionConditionType(t *testing.T) {
	dir := t.TempDir()
	cond := NewPermission(dir)
	cond.Type = PermissionDir
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("dir status = %s, err = %v", result.Status, result.Err)
	}
	cond.Type = PermissionFile
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("file status = %s, want unsatisfied", result.Status)
	}
}

func TestPermissionConditionSymlinkType(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	cond := NewPermission(link)
	cond.Type = PermissionSymlink
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("symlink status = %s, err = %v", result.Status, result.Err)
	}
}

func TestPermissionOwnerMismatchBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := fileOwnerIDs(info)
	if !ok {
		t.Skip("owner IDs unavailable on this platform")
	}
	wrongUID := uid + 1
	cond := NewPermission(path)
	cond.UID = &wrongUID
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("uid mismatch status = %s, want unsatisfied", result.Status)
	}
	wrongGID := gid + 1
	cond = NewPermission(path)
	cond.GID = &wrongGID
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("gid mismatch status = %s, want unsatisfied", result.Status)
	}
}

func TestPermissionValidationAndDescriptor(t *testing.T) {
	noExpectations := NewPermission("path")
	if result := noExpectations.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("no expectations status = %s, want fatal", result.Status)
	}
	noExpectations.Mode = os.ModeSetuid | 0o600
	if result := noExpectations.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("unsupported mode bits status = %s, want fatal", result.Status)
	}
	desc := NewPermission("/tmp/app").Descriptor()
	if desc.Backend != "permission" || desc.Target != "/tmp/app" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestPermissionAdditionalBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := NewPermission("path").Check(ctx); result.Status != CheckUnsatisfied {
		t.Fatalf("cancelled status = %s, want unsatisfied", result.Status)
	}

	unsupportedType := NewPermission("path")
	unsupportedType.Type = "socket"
	if result := unsupportedType.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("unsupported type status = %s, want fatal", result.Status)
	}
	missing := NewPermission(filepath.Join(t.TempDir(), "missing"))
	missing.Type = PermissionFile
	if result := missing.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("missing path status = %s, want unsatisfied", result.Status)
	}
	if result := checkPermissionType("socket", 0); result.Status != CheckFatal {
		t.Fatalf("direct unsupported type status = %s, want fatal", result.Status)
	}
}
