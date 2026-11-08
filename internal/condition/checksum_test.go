package condition

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChecksumConditionMatched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	cond := NewChecksum(path)
	cond.Expected = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestChecksumDescriptor(t *testing.T) {
	desc := NewChecksum("/tmp/app").Descriptor()
	if desc.Backend != "checksum" || desc.Target != "/tmp/app" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestChecksumConditionMismatchAndPrefixConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatch := NewChecksum(path)
	mismatch.Expected = strings.Repeat("0", sha256.Size*2)
	if result := mismatch.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("mismatch status = %s, want unsatisfied", result.Status)
	}
	conflict := NewChecksum(path)
	conflict.Algorithm = ChecksumSHA512
	conflict.Expected = "sha256:" + strings.Repeat("0", sha256.Size*2)
	if result := conflict.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("prefix conflict status = %s, want fatal", result.Status)
	}
}

func TestChecksumAlgorithmsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		algorithm ChecksumAlgorithm
		expected  string
	}{
		{ChecksumSHA1, "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"},
		{ChecksumSHA512, "9b71d224bd62f3785d96d46ad3ea3d73319bfbc2890caadae2dff72519673ca72323c3d99ba5c11d7c7acc6e14b8c5da0c4663475c2e5c3adef46f73bcdec043"},
	}
	for _, tt := range tests {
		cond := NewChecksum(path)
		cond.Algorithm = tt.algorithm
		cond.Expected = tt.expected
		if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
			t.Fatalf("%s status = %s, err = %v", tt.algorithm, result.Status, result.Err)
		}
	}
	auto := NewChecksum(path)
	auto.Expected = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if result := auto.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("prefixed auto status = %s, err = %v", result.Status, result.Err)
	}
	inferred := NewChecksum(path)
	inferred.Expected = "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"
	if result := inferred.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("inferred sha1 status = %s, err = %v", result.Status, result.Err)
	}
	missing := NewChecksum(path)
	if result := missing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing checksum status = %s", result.Status)
	}
	bad := NewChecksum(path)
	bad.Algorithm = "md5"
	bad.Expected = "abc"
	if result := bad.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("bad checksum status = %s", result.Status)
	}
	short := NewChecksum(path)
	short.Algorithm = ChecksumSHA256
	short.Expected = "abcd"
	if result := short.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("short checksum status = %s", result.Status)
	}
}

func TestChecksumAdditionalBranches(t *testing.T) {
	if err := validateChecksumConfig(&ChecksumCondition{}, ChecksumSHA256, strings.Repeat("0", 64)); err == nil {
		t.Fatal("checksum without path succeeded")
	}
	if err := validateChecksumConfig(&ChecksumCondition{Path: "x"}, "md5", strings.Repeat("0", 32)); err == nil {
		t.Fatal("unsupported checksum algorithm succeeded")
	}
	if err := validateChecksumConfig(&ChecksumCondition{Path: "x"}, ChecksumSHA256, ""); err == nil {
		t.Fatal("checksum without expected value succeeded")
	}
	if err := validateChecksumConfig(&ChecksumCondition{Path: "x"}, ChecksumSHA256, "not-hex"); err == nil {
		t.Fatal("invalid checksum hex succeeded")
	}
	if err := validateChecksumConfig(&ChecksumCondition{Path: "x"}, ChecksumSHA256, strings.Repeat("0", 40)); err == nil {
		t.Fatal("wrong checksum length succeeded")
	}
	if _, _, err := resolvedChecksum(&ChecksumCondition{Algorithm: ChecksumSHA256, Expected: "sha1:" + strings.Repeat("0", 40)}); err == nil {
		t.Fatal("checksum prefix mismatch succeeded")
	}
	for _, expected := range []string{strings.Repeat("0", 40), strings.Repeat("0", 128)} {
		if _, err := inferChecksumAlgorithm(expected); err != nil {
			t.Fatalf("inferChecksumAlgorithm(%d chars) error = %v", len(expected), err)
		}
	}
	if _, err := checksumHash("md5"); err == nil {
		t.Fatal("unsupported checksum hash succeeded")
	}
	if _, err := fileChecksum(t.Context(), filepath.Join(t.TempDir(), "missing"), ChecksumSHA256); err == nil {
		t.Fatal("missing checksum file succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := copyWithContext(ctx, io.Discard, strings.NewReader("x")); err == nil {
		t.Fatal("cancelled copy succeeded")
	}
	if _, err := copyWithContext(t.Context(), errorWriter{}, strings.NewReader("x")); err == nil {
		t.Fatal("copy write error succeeded")
	}
}

func (errorWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("write failed")
}

type errorWriter struct{}
