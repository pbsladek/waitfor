package condition

import (
	"context"
	"os/exec"
	"testing"
)

func TestLaunchdConditionRunning(t *testing.T) {
	cond := NewLaunchd("system/com.example.agent")
	cond.Print = func(context.Context, string) (string, error) {
		return "pid = 123\nstate = running\n", nil
	}
	if result := cond.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestLaunchdCheckBranches(t *testing.T) {
	loaded := NewLaunchd("system/com.example.agent")
	loaded.State = LaunchdLoaded
	loaded.Print = func(_ context.Context, label string) (string, error) {
		if label != "system/com.example.agent" {
			t.Fatalf("label = %q", label)
		}
		return "state = waiting\n", nil
	}
	if result := loaded.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("loaded status = %s, err = %v", result.Status, result.Err)
	}
	missing := NewLaunchd("system/com.example.agent")
	missing.Print = func(context.Context, string) (string, error) { return "", exec.ErrNotFound }
	if result := missing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing launchctl status = %s, want fatal", result.Status)
	}
}

func TestLaunchdValidationAndDescriptor(t *testing.T) {
	for _, cond := range []*LaunchdCondition{{}, {Label: "-bad"}, {Label: "svc", State: "booted"}} {
		if err := validateLaunchdConfig(cond); err == nil {
			t.Fatalf("validateLaunchdConfig(%+v) succeeded", cond)
		}
	}
	if result := checkLaunchdRunning("state = waiting\n"); result.Status != CheckUnsatisfied {
		t.Fatalf("running status = %s, want unsatisfied", result.Status)
	}
	desc := NewLaunchd("system/com.example.agent").Descriptor()
	if desc.Backend != "launchd" || desc.Target != "system/com.example.agent" {
		t.Fatalf("descriptor = %+v", desc)
	}
}
