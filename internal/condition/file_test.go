package condition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileConditionStates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ready")

	deleted := NewFile(path, FileDeleted).Check(t.Context())
	if deleted.Status != CheckSatisfied {
		t.Fatalf("deleted Satisfied = false, err = %v", deleted.Err)
	}

	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exists := NewFile(path, FileExists).Check(t.Context())
	if exists.Status != CheckSatisfied {
		t.Fatalf("exists Satisfied = false, err = %v", exists.Err)
	}

	containsCond := NewFile(path, FileExists)
	containsCond.Contains = "ready"
	contains := containsCond.Check(t.Context())
	if contains.Status != CheckSatisfied {
		t.Fatalf("contains Satisfied = false, err = %v", contains.Err)
	}
}

func TestFileConditionNonEmptyWaitsForContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	result := NewFile(path, FileNonEmpty).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("Satisfied = true, want false")
	}
}

func TestFileExistsNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	result := NewFile(path, FileExists).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("exists for missing file should not be satisfied")
	}
}

func TestFileDeletedButStillExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent")
	if err := os.WriteFile(path, []byte("still here"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := NewFile(path, FileDeleted).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("deleted for existing file should not be satisfied")
	}
}

func TestFileContainsNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("nothing here"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewFile(path, FileExists)
	c.Contains = "missing-string"
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("contains for absent substring should not be satisfied")
	}
	if result.Err == nil {
		t.Fatal("Err = nil, want missing substring error")
	}
	if strings.Contains(result.Err.Error(), "missing-string") {
		t.Fatalf("Err = %q leaked requested substring", result.Err)
	}
}

func TestFileContainsDoesNotReadPastLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	file, err := os.Create(path) // #nosec G304 -- test creates this path under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxFileContainsBytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("needle"), maxFileContainsBytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	c := NewFile(path, FileExists)
	c.Contains = "needle"
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("contains past scan limit should not be satisfied")
	}
}

func TestFileContainsRejectsDirectory(t *testing.T) {
	c := NewFile(t.TempDir(), FileExists)
	c.Contains = "needle"
	result := c.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("Status = %s, want %s", result.Status, CheckFatal)
	}
}

func TestFileNonEmptyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost")
	result := NewFile(path, FileNonEmpty).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("nonempty for missing file should not be satisfied")
	}
}

func TestFileDescriptor(t *testing.T) {
	c := NewFile("/tmp/f", FileExists)
	d := c.Descriptor()
	if d.Backend != "file" {
		t.Fatalf("Backend = %q, want file", d.Backend)
	}
	if d.Target != "/tmp/f" {
		t.Fatalf("Target = %q, want /tmp/f", d.Target)
	}
}
