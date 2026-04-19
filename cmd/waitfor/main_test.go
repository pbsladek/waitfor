package main

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestMainSignalCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal behavior differs on windows")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestMainHelperProcess", "--",
		"--timeout", "5s",
		"--interval", "50ms",
		"file", "/tmp/waitfor-signal-test-definitely-missing", "exists",
	)
	cmd.Env = append(os.Environ(), "WAITFOR_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	err = cmd.Wait()
	if err == nil {
		t.Fatal("process exited successfully, want cancellation exit")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("error = %T %v, want *exec.ExitError", err, err)
	}
	// ExitCode() == -1 means the process was killed by a signal (re-raised SIGTERM).
	// ExitCode() == 143 means the fallback os.Exit(143) fired before the signal landed.
	code := exitErr.ExitCode()
	if code != -1 && code != 143 {
		t.Fatalf("exit code = %d, want -1 (signal-killed) or 143 (128+SIGTERM)", code)
	}
}

func TestMainHelperProcess(t *testing.T) {
	if os.Getenv("WAITFOR_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"waitfor"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(2)
}
