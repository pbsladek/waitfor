package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

type fakeCondition struct {
	name         string
	satisfyAfter int
	err          error
	fatal        bool
	mu           sync.Mutex
	attempts     int
}

func (c *fakeCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: c.name}
}

func (c *fakeCondition) Check(ctx context.Context) condition.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.err != nil {
		if c.fatal {
			return condition.Fatal(c.err)
		}
		return condition.Unsatisfied("", c.err)
	}
	if c.satisfyAfter > 0 && c.attempts >= c.satisfyAfter {
		return condition.Satisfied("ready")
	}
	return condition.Unsatisfied("not ready", errors.New("not ready"))
}

func TestRunModeAll(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "a", satisfyAfter: 2},
			&fakeCondition{name: "b", satisfyAfter: 3},
		},
		Timeout:  500 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Satisfied() {
		t.Fatalf("Satisfied = false, outcome = %+v", out)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
	for _, rec := range out.Conditions {
		if !rec.Satisfied {
			t.Fatalf("%s not satisfied", rec.Name)
		}
	}
}

func TestRunModeAnyCancelsAfterFirstSuccess(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "fast", satisfyAfter: 1},
			&fakeCondition{name: "never"},
		},
		Timeout:  500 * time.Millisecond,
		Interval: 20 * time.Millisecond,
		Mode:     ModeAny,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Satisfied() {
		t.Fatalf("Satisfied = false, outcome = %+v", out)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
}

func TestRunTimeout(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{&fakeCondition{name: "never"}},
		Timeout:    20 * time.Millisecond,
		Interval:   5 * time.Millisecond,
		Mode:       ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Satisfied() {
		t.Fatal("Satisfied = true, want false")
	}
	if !out.TimedOut() {
		t.Fatalf("TimedOut = false, outcome = %+v", out)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s", out.Status, StatusTimeout)
	}
}

func TestRunFatal(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "fatal", err: errors.New("bad config"), fatal: true},
		},
		Timeout:  500 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Fatal() {
		t.Fatalf("Fatal = false, outcome = %+v", out)
	}
	if out.Status != StatusFatal {
		t.Fatalf("Status = %s, want %s", out.Status, StatusFatal)
	}
	if out.Satisfied() {
		t.Fatal("Satisfied = true, want false")
	}
}

func TestRunGuardDoesNotBlockModeAllSuccess(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "ready", satisfyAfter: 1},
			condition.NewGuard(&fakeCondition{name: "guard"}),
		},
		Timeout:  500 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusSatisfied, out)
	}
	if !out.Conditions[1].Guard {
		t.Fatal("guard condition was not marked as guard")
	}
}

func TestRunGuardSatisfiedBecomesFatal(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "ready"},
			condition.NewGuard(&fakeCondition{name: "bad-log", satisfyAfter: 1}),
		},
		Timeout:  500 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusFatal {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusFatal, out)
	}
}

func TestRunRequiresNonGuardCondition(t *testing.T) {
	_, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{condition.NewGuard(&fakeCondition{name: "guard"})},
		Timeout:    time.Second,
		Interval:   time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, err := Run(ctx, Config{
		Conditions: []condition.Condition{&fakeCondition{name: "never"}},
		Timeout:    500 * time.Millisecond,
		Interval:   time.Millisecond,
		Mode:       ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusCancelled {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusCancelled, out)
	}
	if !out.Cancelled() {
		t.Fatalf("Cancelled = false, outcome = %+v", out)
	}
}

func TestRunRecordedFatalTakesPrecedenceOverSatisfaction(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			fatalSatisfiedCondition{},
		},
		Timeout:  500 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAny,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusFatal {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusFatal, out)
	}
	if out.Satisfied() {
		t.Fatal("Satisfied = true, want false when a fatal condition is recorded")
	}
}

func TestFinalStatusFatalTakesPrecedenceOverModeAnySatisfaction(t *testing.T) {
	status := finalStatus(t.Context(), []ConditionResult{
		{Name: "ready", Satisfied: true},
		{Name: "fatal", Fatal: true},
	}, ModeAny)
	if status != StatusFatal {
		t.Fatalf("Status = %s, want %s", status, StatusFatal)
	}
}

type fatalSatisfiedCondition struct{}

func (fatalSatisfiedCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "fatal-ready"}
}

func (fatalSatisfiedCondition) Check(ctx context.Context) condition.Result {
	return condition.FatalDetail("ready but invalid", errors.New("bad config"))
}

type contextWaitingCondition struct {
	name     string
	attempts atomic.Int64
}

func (c *contextWaitingCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: c.name}
}

func (c *contextWaitingCondition) Check(ctx context.Context) condition.Result {
	c.attempts.Add(1)
	<-ctx.Done()
	return condition.Unsatisfied("", ctx.Err())
}

func TestRunPerAttemptTimeout(t *testing.T) {
	cond := &contextWaitingCondition{name: "slow"}
	out, err := Run(t.Context(), Config{
		Conditions:        []condition.Condition{cond},
		Timeout:           35 * time.Millisecond,
		Interval:          time.Millisecond,
		PerAttemptTimeout: 5 * time.Millisecond,
		Mode:              ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusTimeout, out)
	}
	if attempts := cond.attempts.Load(); attempts < 2 {
		t.Fatalf("attempts = %d, want multiple attempts before global timeout", attempts)
	}
}

func TestRunRequiresConsecutiveSuccesses(t *testing.T) {
	cond := &fakeCondition{name: "ready", satisfyAfter: 1}
	out, err := Run(t.Context(), Config{
		Conditions:        []condition.Condition{cond},
		Timeout:           500 * time.Millisecond,
		Interval:          time.Millisecond,
		RequiredSuccesses: 3,
		Mode:              ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
	if out.Conditions[0].Attempts < 3 {
		t.Fatalf("attempts = %d, want at least 3", out.Conditions[0].Attempts)
	}
}

func TestRunRequiresStableDuration(t *testing.T) {
	cond := &fakeCondition{name: "ready", satisfyAfter: 1}
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{cond},
		Timeout:    500 * time.Millisecond,
		Interval:   5 * time.Millisecond,
		StableFor:  20 * time.Millisecond,
		Mode:       ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
	if out.Elapsed < 20*time.Millisecond {
		t.Fatalf("elapsed = %s, want at least 20ms", out.Elapsed)
	}
}

func TestRunNormalizesPerAttemptTimeoutToGlobalTimeout(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions:        []condition.Condition{&fakeCondition{name: "ready", satisfyAfter: 1}},
		Timeout:           25 * time.Millisecond,
		Interval:          time.Millisecond,
		PerAttemptTimeout: time.Second,
		Mode:              ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PerAttemptTimeout != 25*time.Millisecond {
		t.Fatalf("PerAttemptTimeout = %s, want 25ms", out.PerAttemptTimeout)
	}
}

func TestOutcomeStatusMethods(t *testing.T) {
	tests := []struct {
		status    Status
		satisfied bool
		timedOut  bool
		cancelled bool
		fatal     bool
	}{
		{status: StatusSatisfied, satisfied: true},
		{status: StatusTimeout, timedOut: true},
		{status: StatusCancelled, cancelled: true},
		{status: StatusFatal, fatal: true},
	}

	for _, tt := range tests {
		out := Outcome{Status: tt.status}
		if out.Satisfied() != tt.satisfied || out.TimedOut() != tt.timedOut || out.Cancelled() != tt.cancelled || out.Fatal() != tt.fatal {
			t.Fatalf("status methods for %s = satisfied:%v timeout:%v cancelled:%v fatal:%v", tt.status, out.Satisfied(), out.TimedOut(), out.Cancelled(), out.Fatal())
		}
	}
}

func TestRunOnAttemptReceivesEachAttempt(t *testing.T) {
	var events int
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{&fakeCondition{name: "eventual", satisfyAfter: 3}},
		Timeout:    500 * time.Millisecond,
		Interval:   time.Millisecond,
		Mode:       ModeAll,
		OnAttempt: func(event AttemptEvent) {
			events++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Satisfied() {
		t.Fatalf("Satisfied = false, outcome = %+v", out)
	}
	if events != out.Conditions[0].Attempts {
		t.Fatalf("events = %d, attempts = %d", events, out.Conditions[0].Attempts)
	}
}

func TestRunRejectsNegativePerAttemptTimeout(t *testing.T) {
	_, err := Run(t.Context(), Config{
		Conditions:        []condition.Condition{&fakeCondition{name: "ready", satisfyAfter: 1}},
		Timeout:           time.Second,
		Interval:          time.Millisecond,
		PerAttemptTimeout: -time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
