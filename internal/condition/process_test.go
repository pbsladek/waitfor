package condition

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestProcessConditionPIDRunningSatisfied(t *testing.T) {
	cond := NewProcess()
	cond.PID = 123
	cond.PIDExists = func(_ context.Context, pid int) (bool, error) {
		if pid != 123 {
			t.Fatalf("pid = %d, want 123", pid)
		}
		return true, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestProcessConditionPIDStoppedSatisfied(t *testing.T) {
	cond := NewProcess()
	cond.PID = 123
	cond.State = ProcessStopped
	cond.PIDExists = func(context.Context, int) (bool, error) {
		return false, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestProcessConditionPIDStateUnsatisfied(t *testing.T) {
	cond := NewProcess()
	cond.PID = 123
	cond.PIDExists = func(context.Context, int) (bool, error) {
		return false, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestProcessConditionNameRunningSatisfied(t *testing.T) {
	cond := NewProcess()
	cond.Name = "postgres"
	cond.List = func(context.Context) ([]ProcessInfo, error) {
		return []ProcessInfo{
			{PID: 1, Name: "/usr/bin/postgres", Command: "/usr/bin/postgres -D data"},
			{PID: 2, Name: "bash", Command: "bash"},
		}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if !strings.Contains(result.Detail, "matched 1") {
		t.Fatalf("detail = %q, want match count", result.Detail)
	}
}

func TestProcessConditionNameStoppedUnsatisfied(t *testing.T) {
	cond := NewProcess()
	cond.Name = "postgres"
	cond.State = ProcessStopped
	cond.List = func(context.Context) ([]ProcessInfo, error) {
		return []ProcessInfo{{PID: 1, Name: "postgres"}}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestProcessConditionListErrorUnsatisfied(t *testing.T) {
	cond := NewProcess()
	cond.Name = "postgres"
	cond.List = func(context.Context) ([]ProcessInfo, error) {
		return nil, errors.New("permission denied")
	}

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestProcessConditionMissingPSFatal(t *testing.T) {
	cond := NewProcess()
	cond.Name = "postgres"
	cond.List = func(context.Context) ([]ProcessInfo, error) {
		return nil, exec.ErrNotFound
	}

	result := cond.Check(t.Context())
	if result.Status != CheckFatal {
		t.Fatalf("status = %s, want fatal", result.Status)
	}
}

func TestProcessConditionContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cond := NewProcess()
	cond.PID = 123

	result := cond.Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", result.Err)
	}
}

func TestProcessConditionInvalidConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*ProcessCondition)
	}{
		{"missing selector", func(*ProcessCondition) {}},
		{"both selectors", func(c *ProcessCondition) { c.PID, c.Name = 1, "postgres" }},
		{"bad state", func(c *ProcessCondition) { c.PID, c.State = 1, "warm" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewProcess()
			tt.setup(cond)
			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestParseProcessTable(t *testing.T) {
	processes := parseProcessTable("  10 /usr/bin/postgres /usr/bin/postgres -D data\nbad line\n  11 bash bash\n")
	if len(processes) != 2 {
		t.Fatalf("len(processes) = %d, want 2", len(processes))
	}
	if processes[0].PID != 10 || processes[0].Name != "/usr/bin/postgres" {
		t.Fatalf("process[0] = %+v", processes[0])
	}
}

func TestProcessDescriptor(t *testing.T) {
	cond := NewProcess()
	cond.Name = "postgres"
	if d := cond.Descriptor(); d.Backend != "process" || d.Target != "postgres" {
		t.Fatalf("descriptor = %+v", d)
	}

	cond.PID = 42
	cond.Name = ""
	if d := cond.Descriptor(); d.Target != "pid 42" {
		t.Fatalf("descriptor target = %q, want pid 42", d.Target)
	}
}

func TestDefaultPIDExistsCurrentProcess(t *testing.T) {
	exists, err := defaultPIDExists(t.Context(), os.Getpid())
	if err != nil {
		t.Fatalf("defaultPIDExists() error = %v", err)
	}
	if !exists {
		t.Fatal("defaultPIDExists() = false, want true for current process")
	}
}

func TestDefaultPIDExistsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exists, err := defaultPIDExists(ctx, os.Getpid())
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if exists {
		t.Fatal("exists = true, want false when context is cancelled")
	}
}

func TestDefaultPIDExistsMissingProcess(t *testing.T) {
	exists, err := defaultPIDExists(t.Context(), 999999)
	if err != nil {
		t.Fatalf("defaultPIDExists() error = %v", err)
	}
	if exists {
		t.Fatal("defaultPIDExists() = true, want false for unlikely pid")
	}
}

func TestDefaultProcessList(t *testing.T) {
	processes, err := defaultProcessList(t.Context())
	if err != nil {
		t.Fatalf("defaultProcessList() error = %v", err)
	}
	if len(processes) == 0 {
		t.Fatal("defaultProcessList() returned no processes")
	}
}

func TestClassifyProcessListError(t *testing.T) {
	if err := classifyProcessListError(errors.New("exit status 1"), context.Canceled); err != context.Canceled {
		t.Fatalf("context error = %v, want context.Canceled", err)
	}
	if err := classifyProcessListError(exec.ErrNotFound, nil); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("not found error = %v, want exec.ErrNotFound", err)
	}
	if err := classifyProcessListError(errors.New("boom"), nil); err == nil || !strings.Contains(err.Error(), "list processes") {
		t.Fatalf("generic error = %v, want list processes", err)
	}
}

func TestFirstCommandTokenEmpty(t *testing.T) {
	if got := firstCommandToken(""); got != "" {
		t.Fatalf("firstCommandToken() = %q, want empty", got)
	}
}
