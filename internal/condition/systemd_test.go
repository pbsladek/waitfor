package condition

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemdConditionActiveSatisfied(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.Show = func(_ context.Context, unit string) (SystemdUnitState, error) {
		if unit != "nginx.service" {
			t.Fatalf("unit = %q, want nginx.service", unit)
		}
		return SystemdUnitState{LoadState: "loaded", ActiveState: "active", SubState: "running"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestSystemdConditionInactiveSatisfied(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.State = SystemdInactive
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{LoadState: "loaded", ActiveState: "inactive", SubState: "dead"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestSystemdConditionFailedSatisfied(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.State = SystemdFailed
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{LoadState: "loaded", ActiveState: "failed", SubState: "failed"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestSystemdConditionStateUnsatisfied(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{LoadState: "loaded", ActiveState: "inactive", SubState: "dead"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if !strings.Contains(result.Detail, "expected active") {
		t.Fatalf("detail = %q, want expected active", result.Detail)
	}
}

func TestSystemdConditionInactiveMissingUnitUnsatisfied(t *testing.T) {
	cond := NewSystemd("missing.service")
	cond.State = SystemdInactive
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{LoadState: "not-found", ActiveState: "inactive"}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestSystemdConditionMissingSystemctlFatal(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{}, exec.ErrNotFound
	}

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestSystemdConditionShowErrorUnsatisfied(t *testing.T) {
	cond := NewSystemd("nginx.service")
	cond.Show = func(context.Context, string) (SystemdUnitState, error) {
		return SystemdUnitState{}, errors.New("unit lookup failed")
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestSystemdConditionContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cond := NewSystemd("nginx.service")

	result := cond.Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", result.Err)
	}
}

func TestSystemdConditionInvalidConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*SystemdCondition)
	}{
		{"missing unit", func(c *SystemdCondition) { c.Unit = "" }},
		{"option-like unit", func(c *SystemdCondition) { c.Unit = "--help" }},
		{"bad state", func(c *SystemdCondition) { c.State = "warm" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewSystemd("nginx.service")
			tt.setup(cond)
			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestParseSystemdShow(t *testing.T) {
	state := parseSystemdShow("LoadState=loaded\nActiveState=active\nSubState=running\n")
	if state.LoadState != "loaded" || state.ActiveState != "active" || state.SubState != "running" {
		t.Fatalf("state = %+v", state)
	}
}

func TestClassifySystemdUnavailableErrorFatal(t *testing.T) {
	err := classifySystemdCommandError(errors.New("exit status 1"), "System has not been booted with systemd", nil)
	if err == nil || !fatalSystemdError(err) {
		t.Fatalf("err = %v, want fatal systemd unavailable", err)
	}
}

func TestClassifySystemdCommandError(t *testing.T) {
	if err := classifySystemdCommandError(errors.New("exit status 1"), "", context.Canceled); err != context.Canceled {
		t.Fatalf("context error = %v, want context.Canceled", err)
	}
	if err := classifySystemdCommandError(exec.ErrNotFound, "", nil); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("not found error = %v, want exec.ErrNotFound", err)
	}
	if err := classifySystemdCommandError(errors.New("exit status 1"), "", nil); err == nil || !strings.Contains(err.Error(), "systemctl show failed") {
		t.Fatalf("empty error = %v, want systemctl show failed", err)
	}
	if err := classifySystemdCommandError(errors.New("exit status 1"), "Access denied", nil); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("output error = %v, want command output", err)
	}
}

func TestDefaultSystemdShowUsesSystemctlOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "systemctl")
	script := "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- test helper creates a private executable under t.TempDir().
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	state, err := defaultSystemdShow(t.Context(), "nginx.service")
	if err != nil {
		t.Fatalf("defaultSystemdShow() error = %v", err)
	}
	if state.ActiveState != "active" || state.SubState != "running" {
		t.Fatalf("state = %+v", state)
	}
}

func TestSystemdDescriptor(t *testing.T) {
	d := NewSystemd("nginx.service").Descriptor()
	if d.Backend != "systemd" || d.Target != "nginx.service" {
		t.Fatalf("descriptor = %+v", d)
	}
}
