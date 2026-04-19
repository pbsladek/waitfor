package runner

import (
	"context"
	"errors"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
	"golang.org/x/sync/errgroup"
)

type Mode string

const (
	ModeAll Mode = "all"
	ModeAny Mode = "any"
)

type Status string

const (
	StatusSatisfied Status = "satisfied"
	StatusTimeout   Status = "timeout"
	StatusCancelled Status = "cancelled"
	StatusFatal     Status = "fatal"
)

type Config struct {
	Conditions        []condition.Condition
	Timeout           time.Duration
	Interval          time.Duration
	PerAttemptTimeout time.Duration
	Mode              Mode
	OnAttempt         func(AttemptEvent)
}

type AttemptEvent struct {
	Name      string
	Attempt   int
	Satisfied bool
	Detail    string
	Error     string
	Elapsed   time.Duration
}

type ConditionResult struct {
	Backend   string
	Target    string
	Name      string
	Satisfied bool
	Attempts  int
	Elapsed   time.Duration
	Detail    string
	LastError string
	Fatal     bool
}

type Outcome struct {
	Status            Status
	Mode              Mode
	Elapsed           time.Duration
	Timeout           time.Duration
	Interval          time.Duration
	PerAttemptTimeout time.Duration
	Conditions        []ConditionResult
}

func (o Outcome) Satisfied() bool {
	return o.Status == StatusSatisfied
}

func (o Outcome) TimedOut() bool {
	return o.Status == StatusTimeout
}

func (o Outcome) Cancelled() bool {
	return o.Status == StatusCancelled
}

func (o Outcome) Fatal() bool {
	return o.Status == StatusFatal
}

func validateRunConfig(cfg Config) error {
	if len(cfg.Conditions) == 0 {
		return errors.New("at least one condition is required")
	}
	if cfg.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if cfg.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if cfg.PerAttemptTimeout < 0 {
		return errors.New("per-attempt timeout cannot be negative")
	}
	return nil
}

func finalStatus(ctx context.Context, records []ConditionResult, mode Mode) Status {
	for _, rec := range records {
		if rec.Fatal {
			return StatusFatal
		}
	}
	if outcomeSatisfied(records, mode) {
		return StatusSatisfied
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusTimeout
	}
	return StatusCancelled
}

func Run(ctx context.Context, cfg Config) (Outcome, error) {
	if err := validateRunConfig(cfg); err != nil {
		return Outcome{}, err
	}
	if cfg.PerAttemptTimeout > cfg.Timeout {
		cfg.PerAttemptTimeout = cfg.Timeout
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	records := make([]ConditionResult, len(cfg.Conditions))
	for i, cond := range cfg.Conditions {
		desc := cond.Descriptor()
		records[i].Backend = desc.Backend
		records[i].Target = desc.Target
		records[i].Name = desc.DisplayName()
	}

	g, runCtx := errgroup.WithContext(ctx)
	for i, cond := range cfg.Conditions {
		i := i
		cond := cond
		g.Go(func() error {
			runCondition(runCtx, cond, cfg, start, &records[i], cancel)
			return nil
		})
	}
	_ = g.Wait()

	out := Outcome{
		Mode:              cfg.Mode,
		Elapsed:           time.Since(start),
		Timeout:           cfg.Timeout,
		Interval:          cfg.Interval,
		PerAttemptTimeout: cfg.PerAttemptTimeout,
		Conditions:        append([]ConditionResult(nil), records...),
		Status:            finalStatus(ctx, records, cfg.Mode),
	}
	return out, nil
}

// makeAttemptContext returns a child context with a per-attempt deadline.
// When timeout is 0, it returns ctx and a no-op cancel.
func makeAttemptContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

// updateRecord writes a single check result into the per-condition record.
func updateRecord(record *ConditionResult, result condition.Result, conditionStart, globalStart time.Time) {
	record.Elapsed = time.Since(conditionStart)
	record.Detail = result.Detail
	if result.Err != nil {
		record.LastError = result.Err.Error()
	}
	if result.Status == condition.CheckFatal {
		record.Fatal = true
	}
	if result.Status == condition.CheckSatisfied {
		record.Satisfied = true
	}
}

// buildAttemptEvent constructs the callback payload for one check attempt.
func buildAttemptEvent(record *ConditionResult, attempt int, result condition.Result, globalStart time.Time) AttemptEvent {
	event := AttemptEvent{
		Name:      record.Name,
		Attempt:   attempt,
		Satisfied: result.Status == condition.CheckSatisfied,
		Detail:    result.Detail,
		Elapsed:   time.Since(globalStart),
	}
	if result.Err != nil {
		event.Error = result.Err.Error()
	}
	return event
}

// waitInterval blocks until the poll interval elapses or ctx is cancelled.
// Returns true if the interval completed normally, false if ctx was cancelled.
func waitInterval(ctx context.Context, timer *time.Timer, interval time.Duration) bool {
	timer.Reset(interval)
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return false
	}
}

func runCondition(
	ctx context.Context,
	cond condition.Condition,
	cfg Config,
	start time.Time,
	record *ConditionResult,
	cancel context.CancelFunc,
) {
	conditionStart := time.Now()
	timer := time.NewTimer(cfg.Interval)
	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		record.Attempts++
		attempt := record.Attempts

		attemptCtx, attemptCancel := makeAttemptContext(ctx, cfg.PerAttemptTimeout)
		result := cond.Check(attemptCtx)
		attemptCancel()

		updateRecord(record, result, conditionStart, start)
		if cfg.OnAttempt != nil {
			cfg.OnAttempt(buildAttemptEvent(record, attempt, result, start))
		}

		switch result.Status {
		case condition.CheckFatal:
			cancel()
			return
		case condition.CheckSatisfied:
			if cfg.Mode == ModeAny {
				cancel()
			}
			return
		}

		if !waitInterval(ctx, timer, cfg.Interval) {
			return
		}
	}
}

func outcomeSatisfied(records []ConditionResult, mode Mode) bool {
	if mode == ModeAny {
		for _, rec := range records {
			if rec.Satisfied {
				return true
			}
		}
		return false
	}
	for _, rec := range records {
		if !rec.Satisfied {
			return false
		}
	}
	return true
}
