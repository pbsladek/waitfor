//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestMainSignalCancellation(t *testing.T) {
	tests := []struct {
		name         string
		signal       syscall.Signal
		fallbackCode int
	}{
		{name: "sigint", signal: syscall.SIGINT, fallbackCode: 130},
		{name: "sigterm", signal: syscall.SIGTERM, fallbackCode: 143},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=TestMainHelperProcess", "--", // #nosec G204 -- test re-executes the current test binary as a controlled helper process.
				"--timeout", "5s",
				"--interval", "50ms",
				"file", "/tmp/waitfor-signal-test-definitely-missing", "--exists",
			)
			cmd.Env = append(os.Environ(), "WAITFOR_HELPER_PROCESS=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			time.Sleep(100 * time.Millisecond)
			if err := cmd.Process.Signal(tt.signal); err != nil {
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
			// ExitCode() == -1 means the process was killed by a re-raised signal.
			// The fallback code means os.Exit(128+signal) fired before the signal landed.
			code := exitErr.ExitCode()
			if code != -1 && code != tt.fallbackCode {
				t.Fatalf("exit code = %d, want -1 (signal-killed) or %d", code, tt.fallbackCode)
			}
		})
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

func TestSignalHelpers(t *testing.T) {
	signals := terminationSignals()
	if len(signals) != 2 {
		t.Fatalf("signals = %v, want 2", signals)
	}
	ignoreBrokenPipe()
}
