package condition

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveConditionZipAndTar(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "app.zip")
	writeZipArchive(t, zipPath, "bin/app")
	zipCond := NewArchive(zipPath)
	zipCond.Member = "bin/app"
	if result := zipCond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("zip status = %s, err = %v", result.Status, result.Err)
	}

	tarPath := filepath.Join(dir, "app.tar")
	writeTarArchive(t, tarPath, "share/app.txt")
	tarCond := NewArchive(tarPath)
	tarCond.Member = "share/app.txt"
	if result := tarCond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("tar status = %s, err = %v", result.Status, result.Err)
	}
}

func TestArchiveDescriptor(t *testing.T) {
	desc := NewArchive("/tmp/app.zip").Descriptor()
	if desc.Backend != "archive" || desc.Target != "/tmp/app.zip" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestArchiveConditionTgzAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.tgz")
	writeTgzArchive(t, path, "share/app.txt")
	cond := NewArchive(path)
	cond.Member = "missing.txt"
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	cond.Member = "share/app.txt"
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("tgz status = %s, err = %v", result.Status, result.Err)
	}
}

func TestArchiveConditionMatchesGlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.zip")
	writeZipArchive(t, path, "bin/app")
	cond := NewArchive(path)
	cond.Matches = "bin/*"
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("glob status = %s, err = %v", result.Status, result.Err)
	}
}

func TestArchiveValidationBranches(t *testing.T) {
	cond := NewArchive("")
	cond.Member = "file"
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing archive path status = %s", result.Status)
	}
	cond = NewArchive("archive.zip")
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing archive member status = %s", result.Status)
	}
	cond.Member = "file"
	cond.Format = "rar"
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("bad archive format status = %s", result.Status)
	}
	cond = NewArchive("archive.zip")
	cond.Matches = "["
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("bad archive glob status = %s", result.Status)
	}
}

func TestArchiveHeaderBudgetValidation(t *testing.T) {
	if err := validateArchiveHeaderBudget(&tar.Header{Size: 1}, maxArchiveEntries+1, 0); err == nil {
		t.Fatal("too many archive entries succeeded")
	}
	if err := validateArchiveHeaderBudget(&tar.Header{Size: -1}, 1, 0); err == nil {
		t.Fatal("negative archive member size succeeded")
	}
	if err := validateArchiveHeaderBudget(&tar.Header{Size: maxArchiveMemberBytes + 1}, 1, 0); err == nil {
		t.Fatal("oversized archive member succeeded")
	}
	if err := validateArchiveHeaderBudget(&tar.Header{Size: 2}, 1, maxArchiveScannedBytes-1); err == nil {
		t.Fatal("oversized archive scan total succeeded")
	}
}

func TestArchiveAdditionalBranches(t *testing.T) {
	both := NewArchive("archive.zip")
	both.Member = "bin/app"
	both.Matches = "bin/*"
	if err := validateArchiveConfig(both); err == nil {
		t.Fatal("archive contains and matches succeeded")
	}
	badFormat := NewArchive("archive.zip")
	badFormat.Member = "bin/app"
	badFormat.Format = "rar"
	if err := validateArchiveConfig(badFormat); err == nil {
		t.Fatal("bad archive format succeeded")
	}
	badGlob := NewArchive("archive.zip")
	badGlob.Matches = "["
	if err := validateArchiveConfig(badGlob); err == nil {
		t.Fatal("bad archive glob succeeded")
	}
	if _, err := archiveContains(t.Context(), badFormat); err == nil {
		t.Fatal("unsupported archive format succeeded")
	}
	if got := resolveArchiveFormat("app.tar.gz", ArchiveAuto); got != ArchiveTgz {
		t.Fatalf("tar.gz format = %s, want tgz", got)
	}
	path := filepath.Join(t.TempDir(), "app.zip")
	writeZipArchive(t, path, "bin/app")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	zipCond := NewArchive(path)
	zipCond.Member = "bin/app"
	if _, err := zipContains(cancelled, zipCond); err == nil {
		t.Fatal("cancelled zip scan succeeded")
	}
	badTgz := filepath.Join(t.TempDir(), "bad.tgz")
	if err := os.WriteFile(badTgz, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgzCond := NewArchive(badTgz)
	tgzCond.Member = "bin/app"
	if _, err := tarContains(t.Context(), tgzCond, true); err == nil {
		t.Fatal("bad tgz succeeded")
	}
	emptyTar := filepath.Join(t.TempDir(), "empty.tar")
	writeEmptyTarArchive(t, emptyTar)
	tarCond := NewArchive(emptyTar)
	tarCond.Member = "missing"
	found, err := tarContains(t.Context(), tarCond, false)
	if err != nil || found {
		t.Fatalf("empty tar found=%v err=%v, want false nil", found, err)
	}
}

func writeZipArchive(t *testing.T, path, name string) {
	t.Helper()
	// #nosec G304 -- callers provide paths rooted in t.TempDir for test fixtures.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := zip.NewWriter(file)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTarArchive(t *testing.T, path, name string) {
	t.Helper()
	// #nosec G304 -- callers provide paths rooted in t.TempDir for test fixtures.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := tar.NewWriter(file)
	body := []byte("ok")
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeEmptyTarArchive(t *testing.T, path string) {
	t.Helper()
	// #nosec G304 -- callers provide paths rooted in t.TempDir for test fixtures.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := tar.NewWriter(file)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTgzArchive(t *testing.T, path, name string) {
	t.Helper()
	// #nosec G304 -- callers provide paths rooted in t.TempDir for test fixtures.
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	body := []byte("ok")
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanArchiveName(t *testing.T) {
	if got := cleanArchiveName("/a/../b/file.txt"); got != "b/file.txt" && !strings.HasSuffix(got, "b/file.txt") {
		t.Fatalf("cleanArchiveName = %q", got)
	}
}
