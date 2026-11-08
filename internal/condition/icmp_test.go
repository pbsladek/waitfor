package condition

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

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

func TestICMPCheckBranchesAndDescriptor(t *testing.T) {
	denied := NewICMP("127.0.0.1")
	denied.Ping = func(context.Context, string) error { return errors.New("host unreachable") }
	if result := denied.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("denied status = %s, want unsatisfied", result.Status)
	}
	missing := NewICMP("127.0.0.1")
	missing.Ping = func(context.Context, string) error { return exec.ErrNotFound }
	if result := missing.Check(t.Context()); result.Status != CheckFatal {
		t.Fatalf("missing ping status = %s, want fatal", result.Status)
	}
	desc := NewICMP("127.0.0.1").Descriptor()
	if desc.Backend != "icmp" || desc.Target != "127.0.0.1" {
		t.Fatalf("descriptor = %+v", desc)
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
	if err := validateICMPConfig(&ICMPCondition{Host: "", Count: 1}); err == nil {
		t.Fatal("blank icmp host succeeded")
	}
	if err := validateICMPConfig(&ICMPCondition{Host: "127.0.0.1", Count: 0}); err == nil {
		t.Fatal("zero icmp count succeeded")
	}
	if err := validateICMPConfig(&ICMPCondition{Host: "127.0.0.1", Count: 1, AttemptTimeout: -time.Second}); err == nil {
		t.Fatal("negative icmp timeout succeeded")
	}
	if timeoutMillis(0) != 1000 || timeoutMillis(time.Nanosecond) != 1 {
		t.Fatal("timeoutMillis default/minimum branch failed")
	}
	if timeoutSeconds(0) != 1 || timeoutSeconds(time.Millisecond) != 1 {
		t.Fatal("timeoutSeconds default/minimum branch failed")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
