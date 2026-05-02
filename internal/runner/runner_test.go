package runner

import (
	"context"
	"errors"
	"math"
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

func TestRunReadySuccessWinsOverLaterGuardAlreadyInFlight(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			delayedSatisfiedCondition{name: "ready"},
			condition.NewGuard(delayedSatisfiedCondition{name: "late-guard", delay: 20 * time.Millisecond, ignoreContext: true}),
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
	for _, rec := range out.Conditions {
		if rec.Guard && rec.Fatal {
			t.Fatalf("late guard recorded fatal after readiness completed: %+v", rec)
		}
	}
}

func TestRunIgnoresSatisfiedResultAfterTimeout(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			delayedSatisfiedCondition{name: "late-ready", delay: 20 * time.Millisecond, ignoreContext: true},
		},
		Timeout:  5 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusTimeout, out)
	}
	if out.Conditions[0].Satisfied {
		t.Fatalf("late satisfied result was recorded: %+v", out.Conditions[0])
	}
}

func TestRunIgnoresFatalResultAfterTimeout(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			delayedFatalCondition{name: "late-fatal", delay: 20 * time.Millisecond},
		},
		Timeout:  5 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s, outcome = %+v", out.Status, StatusTimeout, out)
	}
	if out.Conditions[0].Fatal {
		t.Fatalf("late fatal result was recorded: %+v", out.Conditions[0])
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

func TestFinalStatusUsesFirstTerminalStatus(t *testing.T) {
	records := []ConditionResult{
		{Name: "ready", Satisfied: true},
		{Name: "fatal", Fatal: true},
	}
	if status := finalStatus(t.Context(), records, ModeAny, terminalSatisfied); status != StatusSatisfied {
		t.Fatalf("terminal satisfied Status = %s, want %s", status, StatusSatisfied)
	}
	if status := finalStatus(t.Context(), records, ModeAny, terminalFatal); status != StatusFatal {
		t.Fatalf("terminal fatal Status = %s, want %s", status, StatusFatal)
	}
	if status := finalStatus(t.Context(), records, ModeAny, terminalNone); status != StatusFatal {
		t.Fatalf("no terminal Status = %s, want %s", status, StatusFatal)
	}
}

type fatalSatisfiedCondition struct{}

func (fatalSatisfiedCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "fatal-ready"}
}

func (fatalSatisfiedCondition) Check(ctx context.Context) condition.Result {
	return condition.FatalDetail("ready but invalid", errors.New("bad config"))
}

type delayedSatisfiedCondition struct {
	name          string
	delay         time.Duration
	ignoreContext bool
}

func (c delayedSatisfiedCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: c.name}
}

func (c delayedSatisfiedCondition) Check(ctx context.Context) condition.Result {
	if c.delay > 0 {
		if c.ignoreContext {
			time.Sleep(c.delay)
		} else {
			select {
			case <-time.After(c.delay):
			case <-ctx.Done():
				return condition.Unsatisfied("", ctx.Err())
			}
		}
	}
	return condition.Satisfied("ready")
}

type delayedFatalCondition struct {
	name  string
	delay time.Duration
}

func (c delayedFatalCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: c.name}
}

func (c delayedFatalCondition) Check(ctx context.Context) condition.Result {
	time.Sleep(c.delay)
	return condition.Fatal(errors.New("bad config"))
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

func TestRunRecordsBackoffConfig(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions:  []condition.Condition{&fakeCondition{name: "ready", satisfyAfter: 1}},
		Timeout:     25 * time.Millisecond,
		Interval:    time.Millisecond,
		MaxInterval: 5 * time.Millisecond,
		Backoff:     BackoffExponential,
		Jitter:      0.25,
		Mode:        ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Backoff != BackoffExponential || out.MaxInterval != 5*time.Millisecond || out.Jitter != 0.25 {
		t.Fatalf("backoff config = %s/%s/%v", out.Backoff, out.MaxInterval, out.Jitter)
	}
}

func TestPollScheduleExponentialIntervals(t *testing.T) {
	schedule := newPollSchedule(Config{
		Interval:    10 * time.Millisecond,
		MaxInterval: 25 * time.Millisecond,
		Backoff:     BackoffExponential,
	})
	got := []time.Duration{
		schedule.next(false),
		schedule.next(false),
		schedule.next(false),
		schedule.next(true),
	}
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		25 * time.Millisecond,
		10 * time.Millisecond,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interval[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestPollScheduleJitterRange(t *testing.T) {
	schedule := newPollSchedule(Config{
		Interval:    100 * time.Millisecond,
		MaxInterval: 100 * time.Millisecond,
		Backoff:     BackoffConstant,
		Jitter:      0.5,
	})
	got := schedule.next(false)
	if got < 50*time.Millisecond || got > 150*time.Millisecond {
		t.Fatalf("jittered interval = %s, want within 50ms..150ms", got)
	}
}

func TestDurationHelpers(t *testing.T) {
	if got := minDuration(2*time.Second, time.Second); got != time.Second {
		t.Fatalf("minDuration = %s, want 1s", got)
	}
	if got := maxDuration(time.Nanosecond, time.Millisecond); got != time.Millisecond {
		t.Fatalf("maxDuration = %s, want 1ms", got)
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

func TestRunWaitsForOnAttemptBeforeReturning(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	var out Outcome
	var err error

	go func() {
		defer close(done)
		out, err = Run(t.Context(), Config{
			Conditions: []condition.Condition{&fakeCondition{name: "ready", satisfyAfter: 1}},
			Timeout:    500 * time.Millisecond,
			Interval:   time.Millisecond,
			Mode:       ModeAll,
			OnAttempt: func(event AttemptEvent) {
				close(started)
				<-release
			},
		})
	}()

	select {
	case <-started:
	case <-time.After(100 * time.Millisecond):
		close(release)
		t.Fatal("OnAttempt was not called")
	}
	select {
	case <-done:
		close(release)
		t.Fatal("Run returned before OnAttempt completed")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run did not return after OnAttempt completed")
	}
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
}

func TestRunSerializesOnAttemptCallback(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	conditions := make([]condition.Condition, 20)
	for i := range conditions {
		conditions[i] = &fakeCondition{name: "ready", satisfyAfter: 1}
	}

	out, err := Run(t.Context(), Config{
		Conditions: conditions,
		Timeout:    500 * time.Millisecond,
		Interval:   time.Millisecond,
		Mode:       ModeAll,
		OnAttempt: func(event AttemptEvent) {
			activeNow := active.Add(1)
			for {
				max := maxActive.Load()
				if activeNow <= max || maxActive.CompareAndSwap(max, activeNow) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusSatisfied {
		t.Fatalf("Status = %s, want %s", out.Status, StatusSatisfied)
	}
	if max := maxActive.Load(); max > 1 {
		t.Fatalf("max concurrent OnAttempt callbacks = %d, want <= 1", max)
	}
}

type concurrentCheckCondition struct {
	active atomic.Int32
	max    atomic.Int32
}

func (c *concurrentCheckCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "shared"}
}

func (c *concurrentCheckCondition) Check(ctx context.Context) condition.Result {
	active := c.active.Add(1)
	for {
		max := c.max.Load()
		if active <= max || c.max.CompareAndSwap(max, active) {
			break
		}
	}
	defer c.active.Add(-1)
	select {
	case <-time.After(2 * time.Millisecond):
	case <-ctx.Done():
		return condition.Unsatisfied("", ctx.Err())
	}
	return condition.Unsatisfied("not ready", errors.New("not ready"))
}

func TestRunSerializesReusedConditionInstance(t *testing.T) {
	shared := &concurrentCheckCondition{}
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{shared, condition.NewGuard(shared)},
		Timeout:    25 * time.Millisecond,
		Interval:   time.Millisecond,
		Mode:       ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s", out.Status, StatusTimeout)
	}
	if max := shared.max.Load(); max > 1 {
		t.Fatalf("max concurrent Check calls = %d, want <= 1", max)
	}
}

type passthroughWrapper struct {
	inner condition.Condition
}

func (w *passthroughWrapper) Descriptor() condition.Descriptor {
	return w.inner.Descriptor()
}

func (w *passthroughWrapper) Check(ctx context.Context) condition.Result {
	return w.inner.Check(ctx)
}

func (w *passthroughWrapper) UnwrapCondition() condition.Condition {
	return w.inner
}

func TestRunSerializesConditionsThroughCustomWrapper(t *testing.T) {
	shared := &concurrentCheckCondition{}
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&passthroughWrapper{inner: shared},
			&passthroughWrapper{inner: shared},
		},
		Timeout:  25 * time.Millisecond,
		Interval: time.Millisecond,
		Mode:     ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s", out.Status, StatusTimeout)
	}
	if max := shared.max.Load(); max > 1 {
		t.Fatalf("max concurrent Check calls = %d, want <= 1", max)
	}
}

func TestRunDetectsGuardRoleThroughCustomWrapper(t *testing.T) {
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&fakeCondition{name: "ready", satisfyAfter: 1},
			&passthroughWrapper{inner: condition.NewGuard(&fakeCondition{name: "guard"})},
		},
		Timeout:  50 * time.Millisecond,
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
		t.Fatalf("wrapped guard was not marked as guard: %+v", out.Conditions[1])
	}
}

type blockingCountingCondition struct {
	checks atomic.Int64
}

func (c *blockingCountingCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "blocking"}
}

func (c *blockingCountingCondition) Check(ctx context.Context) condition.Result {
	c.checks.Add(1)
	<-ctx.Done()
	return condition.Unsatisfied("", ctx.Err())
}

func TestRunDoesNotCountGateCanceledWaitAsAttempt(t *testing.T) {
	shared := &blockingCountingCondition{}
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{shared, condition.NewGuard(shared)},
		Timeout:    20 * time.Millisecond,
		Interval:   time.Millisecond,
		Mode:       ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s", out.Status, StatusTimeout)
	}
	attempts := out.Conditions[0].Attempts + out.Conditions[1].Attempts
	if checks := shared.checks.Load(); checks != 1 {
		t.Fatalf("backend checks = %d, want one backend check before timeout", checks)
	}
	if attempts != 0 {
		t.Fatalf("recorded attempts = %d, want late timeout result ignored", attempts)
	}
}

func TestRunPerAttemptTimeoutStartsAfterSharedGate(t *testing.T) {
	shared := &contextWaitingCondition{name: "shared"}
	out, err := Run(t.Context(), Config{
		Conditions: []condition.Condition{
			&passthroughWrapper{inner: shared},
			&passthroughWrapper{inner: shared},
		},
		Timeout:           25 * time.Millisecond,
		Interval:          time.Millisecond,
		PerAttemptTimeout: 2 * time.Millisecond,
		Mode:              ModeAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusTimeout {
		t.Fatalf("Status = %s, want %s", out.Status, StatusTimeout)
	}
	for _, rec := range out.Conditions {
		if rec.Attempts == 0 {
			t.Fatalf("%s attempts = 0, want gate wait not to stop polling; outcome = %+v", rec.Name, out)
		}
	}
}

func TestRunRejectsNilConditions(t *testing.T) {
	var typedNil *fakeCondition
	tests := []struct {
		name string
		cond condition.Condition
	}{
		{name: "nil", cond: nil},
		{name: "typed nil", cond: typedNil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Run(t.Context(), Config{
				Conditions: []condition.Condition{tt.cond},
				Timeout:    time.Second,
				Interval:   time.Millisecond,
				Mode:       ModeAll,
			})
			if err == nil {
				t.Fatal("Run() expected nil condition error, got nil")
			}
		})
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

func TestRunRejectsInvalidBackoffConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "max interval below interval",
			cfg:  Config{MaxInterval: time.Nanosecond},
		},
		{
			name: "invalid backoff",
			cfg:  Config{MaxInterval: time.Millisecond, Backoff: Backoff("linear")},
		},
		{
			name: "invalid mode",
			cfg:  Config{MaxInterval: time.Millisecond, Mode: Mode("first")},
		},
		{
			name: "negative jitter",
			cfg:  Config{MaxInterval: time.Millisecond, Jitter: -0.1},
		},
		{
			name: "nan jitter",
			cfg:  Config{MaxInterval: time.Millisecond, Jitter: math.NaN()},
		},
		{
			name: "infinite jitter",
			cfg:  Config{MaxInterval: time.Millisecond, Jitter: math.Inf(1)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.Conditions = []condition.Condition{&fakeCondition{name: "ready", satisfyAfter: 1}}
			cfg.Timeout = time.Second
			cfg.Interval = time.Millisecond
			_, err := Run(t.Context(), cfg)
			if err == nil {
				t.Fatal("Run() expected error, got nil")
			}
		})
	}
}
