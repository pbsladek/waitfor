package condition

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestLocalBackendValidationBranches(t *testing.T) {
	if result := NewPIDFile("").Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("pidfile missing path status = %s", result.Status)
	}
	badPID := NewPIDFile("pid")
	badPID.State = "waiting"
	if result := badPID.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("pidfile bad state status = %s", result.Status)
	}
	badLock := NewLockfile("lock")
	badLock.State = "busy"
	if result := badLock.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("lock bad state status = %s", result.Status)
	}
	badPermission := NewPermission("path")
	if result := badPermission.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("permission no expectations status = %s", result.Status)
	}
	badPermission.Mode = os.ModeSetuid | 0o600
	if result := badPermission.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("permission mode bits status = %s", result.Status)
	}
}

func TestLocalBackendDescriptors(t *testing.T) {
	tests := []struct {
		cond    Condition
		backend string
		target  string
	}{
		{NewPIDFile("/tmp/app.pid"), "pidfile", "/tmp/app.pid"},
		{NewLockfile("/tmp/app.lock"), "lockfile", "/tmp/app.lock"},
		{NewPermission("/tmp/app"), "permission", "/tmp/app"},
		{NewChecksum("/tmp/app"), "checksum", "/tmp/app"},
		{NewArchive("/tmp/app.zip"), "archive", "/tmp/app.zip"},
		{NewLaunchd("system/com.example.agent"), "launchd", "system/com.example.agent"},
		{NewCosign("ghcr.io/example/app:latest"), "cosign", "ghcr.io/example/app:latest"},
		{NewICMP("127.0.0.1"), "icmp", "127.0.0.1"},
		{NewNTP("time.example"), "ntp", "time.example"},
		{NewGRPC("127.0.0.1:50051"), "grpc", "127.0.0.1:50051"},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			desc := tt.cond.Descriptor()
			if desc.Backend != tt.backend || desc.Target != tt.target {
				t.Fatalf("descriptor = %+v, want backend=%q target=%q", desc, tt.backend, tt.target)
			}
		})
	}
}

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

func TestPermissionConditionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- test needs to assert mode matching above 0600.
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

func TestLaunchdConditionRunning(t *testing.T) {
	cond := NewLaunchd("system/com.example.agent")
	cond.Print = func(context.Context, string) (string, error) {
		return "pid = 123\nstate = running\n", nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestCosignConditionBlobVerification(t *testing.T) {
	cond := NewCosign("artifact")
	cond.Mode = CosignBlob
	cond.Signature = "artifact.sig"
	cond.Verify = func(_ context.Context, got *CosignCondition) error {
		if got.Target != "artifact" || got.Signature != "artifact.sig" {
			t.Fatalf("cosign condition = %+v", got)
		}
		return nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestCommandBackendCheckBranches(t *testing.T) {
	loaded := NewLaunchd("system/com.example.agent")
	loaded.State = LaunchdLoaded
	loaded.Print = func(_ context.Context, label string) (string, error) {
		if label != "system/com.example.agent" {
			t.Fatalf("launchd label = %q", label)
		}
		return "state = waiting\n", nil
	}
	if result := loaded.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("loaded status = %s, err = %v", result.Status, result.Err)
	}
	launchdMissing := NewLaunchd("system/com.example.agent")
	launchdMissing.Print = func(context.Context, string) (string, error) {
		return "", exec.ErrNotFound
	}
	if result := launchdMissing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing launchctl status = %s, want fatal", result.Status)
	}
	cosignDenied := NewCosign("image")
	cosignDenied.Verify = func(context.Context, *CosignCondition) error {
		return errors.New("signature denied")
	}
	if result := cosignDenied.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("cosign denied status = %s, want unsatisfied", result.Status)
	}
	cosignMissing := NewCosign("image")
	cosignMissing.Verify = func(context.Context, *CosignCondition) error {
		return exec.ErrNotFound
	}
	if result := cosignMissing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing cosign status = %s, want fatal", result.Status)
	}
	icmpDenied := NewICMP("127.0.0.1")
	icmpDenied.Ping = func(context.Context, string) error {
		return errors.New("host unreachable")
	}
	if result := icmpDenied.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("icmp denied status = %s, want unsatisfied", result.Status)
	}
	icmpMissing := NewICMP("127.0.0.1")
	icmpMissing.Ping = func(context.Context, string) error {
		return exec.ErrNotFound
	}
	if result := icmpMissing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing ping status = %s, want fatal", result.Status)
	}
}

func TestCommandBackendValidationAndArgs(t *testing.T) {
	if err := validateLaunchdConfig(&LaunchdCondition{}); err == nil {
		t.Fatal("missing launchd label succeeded")
	}
	if err := validateLaunchdConfig(&LaunchdCondition{Label: "-bad"}); err == nil {
		t.Fatal("option-like launchd label succeeded")
	}
	if err := validateLaunchdConfig(&LaunchdCondition{Label: "svc", State: "booted"}); err == nil {
		t.Fatal("bad launchd state succeeded")
	}
	if result := checkLaunchdRunning("state = waiting\n"); result.Status != CheckUnsatisfied {
		t.Fatalf("launchd running status = %s", result.Status)
	}
	cosign := NewCosign("image")
	cosign.Key = "key.pem"
	cosign.Certificate = "cert.pem"
	cosign.Identity = "https://github.com/org/repo/.github/workflows/release.yml@refs/heads/main"
	cosign.OIDCIssuer = "https://token.actions.githubusercontent.com"
	args := cosignArgs(cosign)
	if !strings.Contains(strings.Join(args, " "), "--certificate-identity") || !strings.Contains(strings.Join(args, " "), "--certificate-oidc-issuer") {
		t.Fatalf("cosign args = %v", args)
	}
	if err := validateCosignConfig(&CosignCondition{Target: "blob", Mode: CosignBlob}); err == nil {
		t.Fatal("blob without signature succeeded")
	}
	if err := validateCosignConfig(&CosignCondition{Target: "-keyless", Mode: CosignImage}); err == nil {
		t.Fatal("option-like cosign target succeeded")
	}
}

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

func TestICMPConditionPing(t *testing.T) {
	cond := NewICMP("127.0.0.1")
	cond.Count = 3
	cond.AttemptTimeout = 2 * time.Second
	cond.Ping = func(_ context.Context, host string) error {
		if host != "127.0.0.1" {
			t.Fatalf("host = %q", host)
		}
		return nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestICMPArgsUseCountAndTimeout(t *testing.T) {
	args := pingArgs("127.0.0.1", 3, 2*time.Second)
	if !containsArg(args, "3") {
		t.Fatalf("ping args = %v, want count", args)
	}
	if timeoutMillis(1500*time.Millisecond) != 1500 || timeoutSeconds(1500*time.Millisecond) != 2 {
		t.Fatal("timeout conversion mismatch")
	}
	if err := validateICMPConfig(&ICMPCondition{Host: "-c", Count: 1}); err == nil {
		t.Fatal("option-like icmp host succeeded")
	}
	if err := validateICMPConfig(&ICMPCondition{Host: "127.0.0.1", Count: 1, AttemptTimeout: maxICMPTimeout + time.Second}); err == nil {
		t.Fatal("oversized icmp timeout succeeded")
	}
}

func TestNTPConditionSatisfiedAndQueryError(t *testing.T) {
	cond := NewNTP("time.example:123")
	cond.MaxOffset = time.Second
	cond.Query = func(_ context.Context, address string) (time.Duration, error) {
		if address != "time.example:123" {
			t.Fatalf("ntp address = %q", address)
		}
		return 5 * time.Millisecond, nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("ntp satisfied status = %s, err = %v", result.Status, result.Err)
	}
	failing := NewNTP("time.example:123")
	failing.Query = func(context.Context, string) (time.Duration, error) {
		return 0, errors.New("ntp unavailable")
	}
	if result := failing.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("ntp query error status = %s, want unsatisfied", result.Status)
	}
}

func TestNTPConditionMaxOffset(t *testing.T) {
	cond := NewNTP("time.example:123")
	cond.MaxOffset = time.Second
	cond.Query = func(context.Context, string) (time.Duration, error) {
		return 2 * time.Second, nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestNTPHelpers(t *testing.T) {
	if got := ntpAddress("time.example"); got != "time.example:123" {
		t.Fatalf("ntpAddress = %q", got)
	}
	if got := absDuration(-time.Second); got != time.Second {
		t.Fatalf("absDuration = %s", got)
	}
	if clampInt64ToUint32(-1) != 0 || clampUint64ToUint32(uint64(^uint32(0))+1) != ^uint32(0) {
		t.Fatal("clamp helpers did not clamp")
	}
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	copy(packet[24:32], origin)
	writeNTPTimestamp(packet[32:40], time.Now())
	writeNTPTimestamp(packet[40:48], time.Now())
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err != nil {
		t.Fatalf("parseNTPOffset() error = %v", err)
	}
	packet[0] = 0
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("invalid NTP mode succeeded")
	}
	packet[0] = 0x24
	packet[1] = 16
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("unsynchronized NTP stratum succeeded")
	}
	if err := validateNTPConfig(&NTPCondition{Address: "time.example:bad"}); err == nil {
		t.Fatal("invalid NTP port succeeded")
	}
	if got := ntpAddress("[::1]"); got != "[::1]:123" {
		t.Fatalf("ntpAddress IPv6 = %q", got)
	}
	if err := validateNTPConfig(&NTPCondition{Address: "[not-ip]"}); err == nil {
		t.Fatal("invalid bracketed NTP host succeeded")
	}
}

func TestGRPCConditionCheckUsesHealthRequest(t *testing.T) {
	var seenPath, seenContentType string
	var seenBody []byte
	cond := NewGRPC("127.0.0.1:50051")
	cond.Service = "svc"
	cond.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seenPath = req.URL.Path
		seenContentType = req.Header.Get("Content-Type")
		var err error
		seenBody, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		return grpcTestResponse(1), nil
	})}

	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("grpc status = %s, err = %v", result.Status, result.Err)
	}
	if seenPath != grpcHealthPath || seenContentType != "application/grpc" {
		t.Fatalf("request path/content-type = %q/%q", seenPath, seenContentType)
	}
	if !bytes.Equal(seenBody, grpcFrame(encodeGRPCHealthRequest("svc"))) {
		t.Fatalf("grpc body = %v", seenBody)
	}
	cond.Status = GRPCStatusNotServing
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("grpc mismatch status = %s, want unsatisfied", result.Status)
	}
	failing := NewGRPC("127.0.0.1:50051")
	failing.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	if result := failing.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("grpc transport error status = %s, want unsatisfied", result.Status)
	}
}

func TestGRPCHealthEncodingAndDecoding(t *testing.T) {
	payload := encodeGRPCHealthRequest("svc")
	if !bytes.Equal(payload, []byte{0x0a, 0x03, 's', 'v', 'c'}) {
		t.Fatalf("payload = %v", payload)
	}
	body := grpcFrame([]byte{0x08, 0x01})
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(body)},
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
		Trailer:    http.Header{"Grpc-Status": []string{"0"}},
	}
	status, err := parseGRPCHealthResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if status != GRPCStatusServing {
		t.Fatalf("status = %s, want SERVING", status)
	}
}

func TestGRPCHealthURLAndErrorBranches(t *testing.T) {
	tests := map[string]string{
		"grpc://127.0.0.1:1":  "http://127.0.0.1:1/grpc.health.v1.Health/Check",
		"grpcs://example:443": "https://example:443/grpc.health.v1.Health/Check",
		"127.0.0.1:50051":     "http://127.0.0.1:50051/grpc.health.v1.Health/Check",
	}
	for input, want := range tests {
		got, err := grpcHealthURL(input, false)
		if err != nil {
			t.Fatalf("grpcHealthURL(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("grpcHealthURL(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := grpcHealthURL("missing-port", false); err == nil {
		t.Fatal("missing grpc port succeeded")
	}
	if got, err := grpcHealthURL("example.com:443", true); err != nil || got != "https://example.com:443/grpc.health.v1.Health/Check" {
		t.Fatalf("forced TLS url = %q err=%v", got, err)
	}
	if _, err := parseGRPCFrame([]byte{0}); err == nil {
		t.Fatal("short grpc frame succeeded")
	}
	if _, err := parseGRPCFrame([]byte{1, 0, 0, 0, 0}); err == nil {
		t.Fatal("compressed grpc frame succeeded")
	}
	if status, err := decodeGRPCHealthStatus([]byte{0x08, 0x02}); err != nil || status != GRPCStatusNotServing {
		t.Fatalf("decode status = %s, err = %v", status, err)
	}
	if status, err := decodeGRPCHealthStatus([]byte{0x08, 0x03}); err != nil || status != GRPCStatusServiceUnknown {
		t.Fatalf("decode service unknown = %s, err = %v", status, err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x08, 0x7f}); err == nil {
		t.Fatal("unsupported grpc status succeeded")
	}
	nonOK := &http.Response{StatusCode: http.StatusServiceUnavailable, Body: ioNopCloser{bytes.NewReader(nil)}}
	if _, err := parseGRPCHealthResponse(nonOK); err == nil {
		t.Fatal("non-200 grpc response succeeded")
	}
	trailerErr := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/grpc+proto"}},
		Trailer:    http.Header{"Grpc-Status": []string{"14"}},
	}
	if _, err := parseGRPCHealthResponse(trailerErr); err == nil {
		t.Fatal("grpc trailer error succeeded")
	}
	badContentType := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	if _, err := parseGRPCHealthResponse(badContentType); err == nil {
		t.Fatal("grpc non-grpc content-type succeeded")
	}
	if _, err := grpcHealthURL("httpx://example.com", false); err == nil {
		t.Fatal("invalid grpc URL scheme succeeded")
	}
	if _, err := grpcHealthURL("https://example.com/base?debug=true#frag", false); err == nil {
		t.Fatal("grpc query/fragment URL succeeded")
	}
	if _, err := grpcHealthURL("https://user@example.com/base", false); err == nil {
		t.Fatal("grpc userinfo URL succeeded")
	}
	if got, err := grpcHealthURL("https://example.com/base", false); err != nil || got != "https://example.com/base/grpc.health.v1.Health/Check" {
		t.Fatalf("grpc HTTP URL = %q err=%v", got, err)
	}
	headerStatus := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header: http.Header{
			"Content-Type": []string{"application/grpc"},
			"Grpc-Status":  []string{"7"},
		},
	}
	if _, err := parseGRPCHealthResponse(headerStatus); err == nil {
		t.Fatal("grpc header status error succeeded")
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x12, 0x01, 'x', 0x08, 0x01}); err != nil {
		t.Fatalf("decode with unknown field failed: %v", err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x08, 0x80}); err == nil {
		t.Fatal("malformed grpc status varint succeeded")
	}
	if _, err := decodeGRPCHealthStatus(append([]byte{0x08}, bytes.Repeat([]byte{0x81}, 11)...)); err == nil {
		t.Fatal("overlong grpc status varint succeeded")
	}
	missingStatus := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioNopCloser{bytes.NewReader(grpcFrame([]byte{0x08, 0x01}))},
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
	}
	if _, err := parseGRPCHealthResponse(missingStatus); err == nil {
		t.Fatal("grpc missing status succeeded")
	}
	nilBody := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/grpc"}, "Grpc-Status": []string{"0"}}}
	if _, err := parseGRPCHealthResponse(nilBody); err == nil {
		t.Fatal("grpc nil body succeeded")
	}
}

func TestGRPCValidationAndProtoSkipBranches(t *testing.T) {
	if validateGRPCConfig(&GRPCCondition{}) == nil {
		t.Fatal("missing grpc address succeeded")
	}
	badStatus := NewGRPC("127.0.0.1:1")
	badStatus.Status = "READY"
	if validateGRPCConfig(badStatus) == nil {
		t.Fatal("bad grpc status succeeded")
	}
	badService := NewGRPC("127.0.0.1:1")
	badService.Service = string([]byte{0xff})
	if utf8.ValidString(badService.Service) || validateGRPCConfig(badService) == nil {
		t.Fatal("invalid UTF-8 grpc service succeeded")
	}
	longService := NewGRPC("127.0.0.1:1")
	longService.Service = strings.Repeat("x", maxGRPCServiceNameBytes+1)
	if validateGRPCConfig(longService) == nil {
		t.Fatal("oversized grpc service succeeded")
	}
	tlsCleartext := NewGRPC("grpc://127.0.0.1:1")
	tlsCleartext.UseTLS = true
	if validateGRPCConfig(tlsCleartext) == nil {
		t.Fatal("grpc --tls with cleartext scheme succeeded")
	}

	payload := []byte{
		0x08, 0x05, // field 1 varint first so unsupported health status branch is exercised below
	}
	if _, err := decodeGRPCHealthStatus(payload); err == nil {
		t.Fatal("unsupported grpc health status succeeded")
	}
	withUnknowns := []byte{
		0x11, 1, 2, 3, 4, 5, 6, 7, 8, // fixed64
		0x1a, 0x01, 'x', // length-delimited
		0x25, 1, 2, 3, 4, // fixed32
		0x08, 0x01, // status SERVING
	}
	if status, err := decodeGRPCHealthStatus(withUnknowns); err != nil || status != GRPCStatusServing {
		t.Fatalf("decode with skipped fields status=%s err=%v", status, err)
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x0b}); err == nil {
		t.Fatal("unsupported protobuf wire type succeeded")
	}
	if _, err := decodeGRPCHealthStatus([]byte{0x11, 1}); err == nil {
		t.Fatal("truncated fixed64 field succeeded")
	}
}

func TestWebSocketConditionCheckAgainstLocalServer(t *testing.T) {
	headerC := make(chan string, 1)
	payloadC := make(chan string, 1)
	errC := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			errC <- err
			return
		}
		defer func() { _ = conn.Close() }()
		headerC <- r.Header.Get("Authorization")
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
			errC <- err
			return
		}
		if err := rw.Flush(); err != nil {
			errC <- err
			return
		}
		opcode, payload, err := readWebSocketClientFrame(rw)
		if err != nil {
			errC <- err
			return
		}
		if opcode != websocketOpcodeText {
			errC <- fmt.Errorf("opcode = %d", opcode)
			return
		}
		payloadC <- string(payload)
		if _, err := rw.Write(websocketServerFrame(0x81, []byte("ready-42"))); err != nil {
			errC <- err
			return
		}
		errC <- rw.Flush()
	}))
	t.Cleanup(server.Close)

	cond := NewWebSocket("ws://" + strings.TrimPrefix(server.URL, "http://") + "/events")
	cond.Send = "hello"
	cond.Contains = "ready"
	cond.Headers["Authorization"] = "Bearer token"
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("websocket status = %s, err = %v", result.Status, result.Err)
	}
	if got := receiveString(t, headerC); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := receiveString(t, payloadC); got != "hello" {
		t.Fatalf("payload = %q", got)
	}
	if err := receiveError(t, errC); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketAcceptMatchesRFC6455Example(t *testing.T) {
	got := websocketAccept("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept = %q", got)
	}
}

func TestWebSocketFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(websocketServerFrame(0x81, []byte("ready")))
	frame, err := readWebSocketFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.opcode != 1 || string(frame.payload) != "ready" {
		t.Fatalf("opcode=%d payload=%q", frame.opcode, frame.payload)
	}
	var client bytes.Buffer
	if err := writeWebSocketText(&client, "ready"); err != nil {
		t.Fatal(err)
	}
	raw := client.Bytes()
	if len(raw) < 6 || raw[1]&0x80 == 0 {
		t.Fatalf("client frame was not masked: %v", raw[:minInt(len(raw), 6)])
	}
}

func TestWebSocketHelpers(t *testing.T) {
	if got := websocketAddress(mustURL(t, "ws://example.com/ready")); got != "example.com:80" {
		t.Fatalf("ws address = %q", got)
	}
	if got := websocketAddress(mustURL(t, "wss://example.com/ready")); got != "example.com:443" {
		t.Fatalf("wss address = %q", got)
	}
	if err := validateWebSocketConfig(NewWebSocket("http://example.com")); err == nil {
		t.Fatal("invalid websocket scheme succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws:///ready")); err == nil {
		t.Fatal("missing websocket host succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://user@example.com/ready")); err == nil {
		t.Fatal("websocket userinfo succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://example.com/ready#fragment")); err == nil {
		t.Fatal("websocket fragment succeeded")
	}
	if err := validateWebSocketConfig(NewWebSocket("ws://example.com:bad/ready")); err == nil {
		t.Fatal("websocket bad port succeeded")
	}
	dup := NewWebSocket("ws://example.com/ready")
	dup.Headers["Connection"] = "keep-alive"
	if err := validateWebSocketConfig(dup); err == nil {
		t.Fatal("reserved websocket header succeeded")
	}
	largeSend := NewWebSocket("ws://example.com/ready")
	largeSend.Send = strings.Repeat("x", maxWebSocketSendBytes+1)
	if err := validateWebSocketConfig(largeSend); err == nil {
		t.Fatal("oversized websocket send succeeded")
	}
	largeHeaders := NewWebSocket("ws://example.com/ready")
	largeHeaders.Headers["X-Big"] = strings.Repeat("x", maxWebSocketHeaderBytes+1)
	if err := validateWebSocketConfig(largeHeaders); err == nil {
		t.Fatal("oversized websocket headers succeeded")
	}
	if desc := NewWebSocket("wss://example.com/events?token=secret").Descriptor(); strings.Contains(desc.Target, "secret") {
		t.Fatalf("descriptor leaked query secret: %+v", desc)
	}
	var medium bytes.Buffer
	header := appendWebSocketLength([]byte{0x81}, 126, false)
	medium.Write(header)
	medium.Write(bytes.Repeat([]byte("x"), 126))
	if frame, err := readWebSocketFrame(&medium); err != nil || frame.opcode != 1 || len(frame.payload) != 126 {
		t.Fatalf("medium frame opcode=%d len=%d err=%v", frame.opcode, len(frame.payload), err)
	}
	var large bytes.Buffer
	header = appendWebSocketLength([]byte{0x81}, 66000, false)
	large.Write(header)
	large.Write(bytes.Repeat([]byte("x"), 66000))
	if frame, err := readWebSocketFrame(&large); err != nil || len(frame.payload) != 66000 {
		t.Fatalf("large frame len=%d err=%v", len(frame.payload), err)
	}
	if byteWebSocketLength(200) != 125 {
		t.Fatal("byteWebSocketLength did not clamp")
	}
	if !httpHeaderHasToken(http.Header{"Connection": []string{"keep-alive, Upgrade"}}, "Connection", "upgrade") {
		t.Fatal("Connection token was not detected")
	}
}

func TestWebSocketFrameValidation(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "masked server frame", data: append([]byte{0x81, 0x80, 0, 0, 0, 0}, nil...)},
		{name: "rsv set", data: []byte{0xc1, 0}},
		{name: "reserved opcode", data: []byte{0x83, 0}},
		{name: "fragmented control", data: []byte{0x09, 0}},
		{name: "non-minimal 16-bit", data: []byte{0x81, 126, 0, 1, 'x'}},
		{name: "non-minimal 64-bit", data: []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 126}},
		{name: "64-bit high bit", data: []byte{0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := readWebSocketFrame(bytes.NewReader(tt.data)); err == nil {
				t.Fatal("invalid websocket frame succeeded")
			}
		})
	}
	controlTooLarge := appendWebSocketLength([]byte{0x89}, 126, false)
	controlTooLarge = append(controlTooLarge, bytes.Repeat([]byte("x"), 126)...)
	if _, err := readWebSocketFrame(bytes.NewReader(controlTooLarge)); err == nil {
		t.Fatal("oversized control frame succeeded")
	}
}

func TestWebSocketFragmentedTextAndControls(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(websocketServerFrame(0x01, []byte("rea")))
	buf.Write(websocketServerFrame(0x89, []byte("ping")))
	buf.Write(websocketServerFrame(0x80, []byte("dy")))
	var pong bytes.Buffer
	got, err := readWebSocketTextWithPong(&buf, &pong)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ready" {
		t.Fatalf("message = %q", got)
	}
	if pong.Len() == 0 {
		t.Fatal("ping did not produce pong")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x88, nil))); err == nil {
		t.Fatal("close frame succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x80, []byte("orphan")))); err == nil {
		t.Fatal("orphan continuation succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x82, []byte("binary")))); err == nil {
		t.Fatal("binary frame succeeded")
	}
	if _, _, err := (&websocketMessageState{}).handleFrame(websocketFrame{opcode: 0x0b}, nil); err == nil {
		t.Fatal("unsupported opcode succeeded")
	}
	if _, err := readWebSocketText(bytes.NewReader(websocketServerFrame(0x81, []byte{0xff}))); err == nil {
		t.Fatal("invalid UTF-8 websocket text succeeded")
	}
	if err := writeAll(shortWriter{}, []byte("x")); err == nil {
		t.Fatal("short write succeeded")
	}
}

func TestWebSocketHandshakePreservesBufferedFrameBytes(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	done := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			done <- err
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: keep-alive, Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + websocketAccept(key) + "\r\n\r\n"
		_, err = server.Write(append([]byte(response), websocketServerFrame(0x81, []byte("ready"))...))
		done <- err
	}()
	reader, err := websocketHandshake(client, mustURL(t, "ws://example.com/events"), nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := readWebSocketText(&websocketConn{Conn: client, reader: reader})
	if err != nil {
		t.Fatal(err)
	}
	if message != "ready" {
		t.Fatalf("message = %q", message)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketHandshakeRejectsOversizedHeaders(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("HTTP/1.1 101 Switching Protocols\r\nX-Big: " + strings.Repeat("x", maxWebSocketHandshakeBytes) + "\r\n\r\n"))
	if _, err := readWebSocketHandshakeResponse(reader); err == nil {
		t.Fatal("oversized websocket handshake succeeded")
	}
}

func TestWebSocketRegexAndHeaderRequest(t *testing.T) {
	cond := NewWebSocket("ws://example.com/events")
	cond.Matches = regexp.MustCompile(`ready-\d+`)
	if !cond.Matches.MatchString("ready-42") {
		t.Fatal("regex did not match")
	}
	req := websocketHandshakeRequest("/events", "example.com", "key", map[string]string{"Authorization": "Bearer token"})
	if !strings.Contains(req, "Authorization: Bearer token\r\n") {
		t.Fatalf("request = %q", req)
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

func writeZipArchive(t *testing.T, path, name string) {
	t.Helper()
	file, err := os.Create(path) // #nosec G304 -- test helper writes only caller-provided t.TempDir() paths.
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
	file, err := os.Create(path) // #nosec G304 -- test helper writes only caller-provided t.TempDir() paths.
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

func writeTgzArchive(t *testing.T, path, name string) {
	t.Helper()
	file, err := os.Create(path) // #nosec G304 -- test helper writes only caller-provided t.TempDir() paths.
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

type ioNopCloser struct {
	*bytes.Reader
}

func (c ioNopCloser) Close() error {
	return nil
}

func TestDecodeWebSocketAcceptUsesBase64(t *testing.T) {
	if _, err := base64.StdEncoding.DecodeString(websocketAccept("test")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanArchiveName(t *testing.T) {
	if got := cleanArchiveName("/a/../b/file.txt"); got != "b/file.txt" && !strings.HasSuffix(got, "b/file.txt") {
		t.Fatalf("cleanArchiveName = %q", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestReadNTPTimestampZero(t *testing.T) {
	if got := readNTPTimestamp(make([]byte, 8)); !got.IsZero() {
		t.Fatalf("zero timestamp = %s", got)
	}
}

func TestNTPValidationBranches(t *testing.T) {
	cond := NewNTP("")
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing ntp address status = %s", result.Status)
	}
	cond = NewNTP("time.example")
	cond.MaxOffset = -time.Second
	if result := cond.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("negative ntp offset status = %s", result.Status)
	}
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	copy(packet[24:32], origin)
	if _, err := parseNTPOffset(packet, origin, time.Now(), time.Now()); err == nil {
		t.Fatal("missing NTP timestamps succeeded")
	}
	if clampInt64ToUint32(int64(^uint32(0))+1) != ^uint32(0) {
		t.Fatal("int64 clamp did not cap max")
	}
}

func TestNTPRFCValidationBranches(t *testing.T) {
	origin := make([]byte, 8)
	writeNTPTimestamp(origin, time.Now())
	packet := make([]byte, ntpPacketSize)
	packet[0] = 0x24
	packet[1] = 1
	copy(packet[24:32], origin)
	now := time.Now()
	writeNTPTimestamp(packet[32:40], now)
	writeNTPTimestamp(packet[40:48], now.Add(time.Millisecond))

	tests := []struct {
		name  string
		patch func([]byte)
	}{
		{name: "unsynchronized leap", patch: func(p []byte) { p[0] = 0xe4 }},
		{name: "old version", patch: func(p []byte) { p[0] = 0x14 }},
		{name: "client mode", patch: func(p []byte) { p[0] = 0x23 }},
		{name: "stratum zero", patch: func(p []byte) { p[1] = 0 }},
		{name: "originate mismatch", patch: func(p []byte) { p[24] ^= 0xff }},
		{name: "transmit before receive", patch: func(p []byte) {
			writeNTPTimestamp(p[32:40], now.Add(time.Second))
			writeNTPTimestamp(p[40:48], now)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := append([]byte(nil), packet...)
			tt.patch(candidate)
			if _, err := parseNTPOffset(candidate, origin, now, now.Add(2*time.Millisecond)); err == nil {
				t.Fatal("invalid NTP packet succeeded")
			}
		})
	}
}

func TestAppendVarintMultiByte(t *testing.T) {
	out := appendVarint(nil, 300)
	got, n := readVarint(out)
	if got != 300 || n != len(out) {
		t.Fatalf("varint got=%d n=%d bytes=%v", got, n, out)
	}
}

func TestGRPCUint32LengthClamps(t *testing.T) {
	if uint32Length(-1) != 0 {
		t.Fatal("negative length did not clamp")
	}
	if uint32Length(1<<33) != ^uint32(0) {
		t.Fatal("large length did not clamp")
	}
}

func TestWebSocketReadLengthExtended(t *testing.T) {
	var buf bytes.Buffer
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], 66000)
	buf.Write(raw[:])
	length, masked, err := readWebSocketLength(&buf, 127)
	if err != nil {
		t.Fatal(err)
	}
	if length != 66000 || masked {
		t.Fatalf("length=%d masked=%v", length, masked)
	}
}

func grpcTestResponse(status byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(grpcFrame([]byte{0x08, status}))),
		Header:     http.Header{"Content-Type": []string{"application/grpc"}},
		Trailer:    http.Header{"Grpc-Status": []string{"0"}},
	}
}

func receiveString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for string")
		return ""
	}
}

func receiveError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error")
		return nil
	}
}

func readWebSocketClientFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	switch length {
	case 126:
		var raw [2]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(raw[:]))
	case 127:
		var raw [8]byte
		if _, err := io.ReadFull(r, raw[:]); err != nil {
			return 0, nil, err
		}
		size := binary.BigEndian.Uint64(raw[:])
		if size > uint64(^uint(0)>>1) {
			return 0, nil, fmt.Errorf("client frame too large")
		}
		length = int(size)
	}
	if header[1]&0x80 == 0 {
		return 0, nil, fmt.Errorf("client frame was not masked")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return header[0] & 0x0f, payload, nil
}

func websocketServerFrame(first byte, payload []byte) []byte {
	header := appendWebSocketLength([]byte{first}, len(payload), false)
	return append(header, payload...)
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) {
	return 0, nil
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
