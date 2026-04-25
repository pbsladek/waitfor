//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package condition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecConditionTimeoutKillsUnixProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	script := `nohup sleep 20 >/dev/null 2>&1 & echo $! > "$1"; wait`
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	result := NewExec([]string{"/bin/sh", "-c", script, "sh", pidfile}).Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	pid := readPID(t, pidfile)
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	if processAliveAfter(pid, time.Second) {
		t.Fatalf("process %d survived exec timeout", pid)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- pid file path is created by this test in t.TempDir.
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid file %q: %v", string(raw), err)
	}
	return pid
}

func processAliveAfter(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processAlive(pid)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
