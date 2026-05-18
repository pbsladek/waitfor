package condition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobMinCountSatisfied(t *testing.T) {
	dir := t.TempDir()
	writeGlobFile(t, dir, "a.done")
	writeGlobFile(t, dir, "b.done")

	cond := NewGlob(filepath.Join(dir, "*.done"))
	cond.MinCount = 2
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "2 matches" {
		t.Fatalf("detail = %q, want 2 matches", result.Detail)
	}
}

func TestGlobMinCountUnsatisfied(t *testing.T) {
	dir := t.TempDir()
	writeGlobFile(t, dir, "a.done")

	cond := NewGlob(filepath.Join(dir, "*.done"))
	cond.MinCount = 2
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if !strings.Contains(result.Detail, "need at least 2") {
		t.Fatalf("detail = %q, want min-count detail", result.Detail)
	}
}

func TestGlobAbsent(t *testing.T) {
	cond := NewGlob(filepath.Join(t.TempDir(), "*.done"))
	cond.Absent = true
	cond.MinCount = 0

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "no matches" {
		t.Fatalf("detail = %q, want no matches", result.Detail)
	}
}

func TestGlobAbsentUnsatisfied(t *testing.T) {
	dir := t.TempDir()
	writeGlobFile(t, dir, "a.done")
	cond := NewGlob(filepath.Join(dir, "*.done"))
	cond.Absent = true
	cond.MinCount = 0

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "1 matches still present" {
		t.Fatalf("detail = %q, want still present", result.Detail)
	}
}

func TestGlobMaxCount(t *testing.T) {
	dir := t.TempDir()
	writeGlobFile(t, dir, "a.done")
	writeGlobFile(t, dir, "b.done")

	cond := NewGlob(filepath.Join(dir, "*.done"))
	cond.MaxCount = 1
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if !strings.Contains(result.Detail, "need at most 1") {
		t.Fatalf("detail = %q, want max-count detail", result.Detail)
	}
}

func TestGlobInvalidConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*GlobCondition)
	}{
		{"empty pattern", func(c *GlobCondition) { c.Pattern = "" }},
		{"negative min", func(c *GlobCondition) { c.MinCount = -1 }},
		{"bad max", func(c *GlobCondition) { c.MaxCount = -2 }},
		{"min exceeds max", func(c *GlobCondition) { c.MinCount = 2; c.MaxCount = 1 }},
		{"absent positive min", func(c *GlobCondition) { c.Absent = true; c.MinCount = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewGlob("*.done")
			cond.Glob = func(string) ([]string, error) {
				t.Fatal("glob should not be called for invalid config")
				return nil, nil
			}
			tt.setup(cond)
			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestGlobMalformedPatternFatal(t *testing.T) {
	result := NewGlob("[").Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestBoundedGlobBranches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ready.done")
	writeGlobFile(t, dir, "ready.done")
	matches, err := boundedGlob(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != path {
		t.Fatalf("matches = %v, want exact path", matches)
	}
	if matches, err := boundedGlob(t.Context(), filepath.Join(dir, "missing.done")); err != nil || len(matches) != 0 {
		t.Fatalf("missing matches=%v err=%v, want none", matches, err)
	}
	if _, err := boundedGlob(t.Context(), filepath.Join(dir, "*", "*.done")); err == nil {
		t.Fatal("directory wildcard succeeded")
	}
	names := make([]string, maxGlobMatches+1)
	for i := range names {
		names[i] = fmt.Sprintf("%d.done", i)
	}
	if _, err := appendGlobMatches(nil, dir, "*.done", names); err == nil {
		t.Fatal("too many glob matches succeeded")
	}
}

func TestGlobContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := NewGlob("*.done").Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestGlobDescriptor(t *testing.T) {
	desc := NewGlob("/tmp/*.done").Descriptor()
	if desc.Backend != "glob" || desc.Target != "/tmp/*.done" {
		t.Fatalf("Descriptor() = %+v", desc)
	}
}

func writeGlobFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGlobInjectedErrorFatal(t *testing.T) {
	cond := NewGlob("*.done")
	cond.Glob = func(string) ([]string, error) {
		return nil, fmt.Errorf("glob failed")
	}
	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}
