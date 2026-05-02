package runner

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
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

type Backoff string

const (
	BackoffConstant    Backoff = "constant"
	BackoffExponential Backoff = "exponential"
)

type terminalStatus int32

const (
	terminalNone terminalStatus = iota
	terminalSatisfied
	terminalFatal
)

const maxConditionUnwrapDepth = 32

type Config struct {
	Conditions        []condition.Condition
	Timeout           time.Duration
	Interval          time.Duration
	MaxInterval       time.Duration
	Backoff           Backoff
	Jitter            float64
	PerAttemptTimeout time.Duration
	RequiredSuccesses int
	StableFor         time.Duration
	Mode              Mode
	// OnAttempt is called synchronously after each recorded backend check. The
	// runner serializes callback execution so callers do not need to add locking
	// solely to protect their output writer.
	OnAttempt func(AttemptEvent)
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
	Guard     bool
}

type Outcome struct {
	Status            Status
	Mode              Mode
	Elapsed           time.Duration
	Timeout           time.Duration
	Interval          time.Duration
	MaxInterval       time.Duration
	Backoff           Backoff
	Jitter            float64
	PerAttemptTimeout time.Duration
	RequiredSuccesses int
	StableFor         time.Duration
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

func ValidateConfig(cfg Config) error {
	cfg = normalizeRunConfig(cfg)
	return validateRunConfig(cfg)
}

func validateRunConfig(cfg Config) error {
	if len(cfg.Conditions) == 0 {
		return errors.New("at least one condition is required")
	}
	if err := validateConditionConfig(cfg.Conditions); err != nil {
		return err
	}
	if err := validateTimingConfig(cfg); err != nil {
		return err
	}
	if err := validateBackoffConfig(cfg); err != nil {
		return err
	}
	if err := validateStabilityConfig(cfg); err != nil {
		return err
	}
	if err := validateModeConfig(cfg); err != nil {
		return err
	}
	if !hasReadyCondition(cfg.Conditions) {
		return errors.New("at least one non-guard condition is required")
	}
	return nil
}

func validateConditionConfig(conditions []condition.Condition) error {
	for _, cond := range conditions {
		if isNilCondition(cond) {
			return errors.New("condition cannot be nil")
		}
	}
	return nil
}

func validateTimingConfig(cfg Config) error {
	if cfg.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if cfg.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	if cfg.MaxInterval < cfg.Interval {
		return errors.New("max interval must be greater than or equal to interval")
	}
	if cfg.PerAttemptTimeout < 0 {
		return errors.New("per-attempt timeout cannot be negative")
	}
	return nil
}

func validateBackoffConfig(cfg Config) error {
	if cfg.Backoff != BackoffConstant && cfg.Backoff != BackoffExponential {
		return errors.New("backoff must be constant or exponential")
	}
	if math.IsNaN(cfg.Jitter) || math.IsInf(cfg.Jitter, 0) || cfg.Jitter < 0 || cfg.Jitter > 1 {
		return errors.New("jitter must be between 0 and 1")
	}
	return nil
}

func validateStabilityConfig(cfg Config) error {
	if cfg.RequiredSuccesses < 0 {
		return errors.New("successes cannot be negative")
	}
	if cfg.StableFor < 0 {
		return errors.New("stable-for cannot be negative")
	}
	return nil
}

func validateModeConfig(cfg Config) error {
	switch cfg.Mode {
	case ModeAll, ModeAny:
		return nil
	default:
		return errors.New("mode must be all or any")
	}
}

func normalizeRunConfig(cfg Config) Config {
	if cfg.Backoff == "" {
		cfg.Backoff = BackoffConstant
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeAll
	}
	if cfg.MaxInterval == 0 {
		cfg.MaxInterval = cfg.Interval
	}
	if cfg.RequiredSuccesses == 0 {
		cfg.RequiredSuccesses = 1
	}
	if cfg.PerAttemptTimeout > cfg.Timeout {
		cfg.PerAttemptTimeout = cfg.Timeout
	}
	return cfg
}

func conditionRole(cond condition.Condition) condition.Role {
	for depth := 0; depth < maxConditionUnwrapDepth; depth++ {
		if provider, ok := cond.(condition.RoleProvider); ok {
			return provider.ConditionRole()
		}
		wrapper, ok := cond.(condition.Wrapper)
		if !ok {
			return condition.RoleReady
		}
		inner := wrapper.UnwrapCondition()
		if isNilCondition(inner) {
			return condition.RoleReady
		}
		cond = inner
	}
	return condition.RoleReady
}

func hasReadyCondition(conditions []condition.Condition) bool {
	return readyConditionCount(conditions) > 0
}

func readyConditionCount(conditions []condition.Condition) int {
	count := 0
	for _, cond := range conditions {
		if conditionRole(cond) == condition.RoleReady {
			count++
		}
	}
	return count
}

func finalStatus(ctx context.Context, records []ConditionResult, mode Mode, terminal terminalStatus) Status {
	switch terminal {
	case terminalFatal:
		return StatusFatal
	case terminalSatisfied:
		return StatusSatisfied
	}
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
	cfg = normalizeRunConfig(cfg)
	if err := validateRunConfig(cfg); err != nil {
		return Outcome{}, err
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	records := make([]ConditionResult, len(cfg.Conditions))
	var readyRemaining atomic.Int64
	var terminal atomic.Int32
	readyRemaining.Store(int64(readyConditionCount(cfg.Conditions)))
	for i, cond := range cfg.Conditions {
		desc := cond.Descriptor()
		records[i].Backend = desc.Backend
		records[i].Target = desc.Target
		records[i].Name = desc.DisplayName()
		records[i].Guard = conditionRole(cond) == condition.RoleGuard
	}

	gates := conditionGates(cfg.Conditions)
	attempts := newAttemptRecorder(cfg.OnAttempt)
	g, runCtx := errgroup.WithContext(ctx)
	for i, cond := range cfg.Conditions {
		i := i
		cond := cond
		g.Go(func() error {
			runCondition(runCtx, cond, gates[i], cfg, start, &records[i], cancel, &readyRemaining, &terminal, attempts)
			return nil
		})
	}
	_ = g.Wait()

	out := Outcome{
		Mode:              cfg.Mode,
		Elapsed:           time.Since(start),
		Timeout:           cfg.Timeout,
		Interval:          cfg.Interval,
		MaxInterval:       cfg.MaxInterval,
		Backoff:           cfg.Backoff,
		Jitter:            cfg.Jitter,
		PerAttemptTimeout: cfg.PerAttemptTimeout,
		RequiredSuccesses: cfg.RequiredSuccesses,
		StableFor:         cfg.StableFor,
		Conditions:        append([]ConditionResult(nil), records...),
		Status:            finalStatus(ctx, records, cfg.Mode, terminalStatus(terminal.Load())),
	}
	return out, nil
}

type conditionGate chan struct{}

func conditionGates(conditions []condition.Condition) []conditionGate {
	byPointer := map[uintptr]conditionGate{}
	gates := make([]conditionGate, len(conditions))
	for i, cond := range conditions {
		key, ok := conditionPointerKey(cond)
		if ok {
			gate, exists := byPointer[key]
			if !exists {
				gate = make(conditionGate, 1)
				byPointer[key] = gate
			}
			gates[i] = gate
			continue
		}
		gates[i] = make(conditionGate, 1)
	}
	return gates
}

func conditionPointerKey(cond condition.Condition) (uintptr, bool) {
	cond = unwrapCondition(cond)
	if isNilCondition(cond) {
		return 0, false
	}
	value := reflect.ValueOf(cond)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return 0, false
	}
	return value.Pointer(), true
}

func unwrapCondition(cond condition.Condition) condition.Condition {
	for depth := 0; depth < maxConditionUnwrapDepth; depth++ {
		wrapper, ok := cond.(condition.Wrapper)
		if !ok {
			return cond
		}
		inner := wrapper.UnwrapCondition()
		if isNilCondition(inner) {
			return cond
		}
		cond = inner
	}
	return cond
}

func isNilCondition(cond condition.Condition) bool {
	if cond == nil {
		return true
	}
	value := reflect.ValueOf(cond)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func checkCondition(ctx context.Context, cond condition.Condition, gate conditionGate, timeout time.Duration) (condition.Result, bool) {
	release, ok := acquireConditionGate(ctx, gate)
	if !ok {
		return condition.Unsatisfied("", ctx.Err()), false
	}
	defer release()

	attemptCtx, attemptCancel := makeAttemptContext(ctx, timeout)
	result := cond.Check(attemptCtx)
	attemptErr := attemptCtx.Err()
	attemptCancel()

	if ctx.Err() != nil {
		return condition.Unsatisfied("", ctx.Err()), false
	}
	if attemptErr != nil {
		return condition.Unsatisfied("", attemptErr), true
	}
	return result, true
}

func acquireConditionGate(ctx context.Context, gate conditionGate) (func(), bool) {
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, true
	case <-ctx.Done():
		return nil, false
	}
}

type attemptRecorder struct {
	on func(AttemptEvent)
	mu sync.Mutex
}

func newAttemptRecorder(on func(AttemptEvent)) *attemptRecorder {
	if on == nil {
		return nil
	}
	return &attemptRecorder{on: on}
}

func (r *attemptRecorder) emit(event AttemptEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on(event)
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
	} else {
		record.LastError = ""
	}
	if result.Status == condition.CheckFatal {
		record.Fatal = true
	}
	if result.Status == condition.CheckSatisfied {
		record.Satisfied = true
	}
}

type stabilityProgress struct {
	consecutive int
	stableSince time.Time
}

func (p *stabilityProgress) reset() {
	p.consecutive = 0
	p.stableSince = time.Time{}
}

func applyStabilityThreshold(result condition.Result, cfg Config, progress *stabilityProgress, now time.Time) condition.Result {
	if result.Status != condition.CheckSatisfied {
		progress.reset()
		return result
	}
	progress.consecutive++
	if progress.stableSince.IsZero() {
		progress.stableSince = now
	}
	if stabilitySatisfied(cfg, progress, now) {
		return result
	}
	return condition.Unsatisfied(stabilityDetail(cfg, progress, now), errors.New("stability threshold not met"))
}

func stabilitySatisfied(cfg Config, progress *stabilityProgress, now time.Time) bool {
	if progress.consecutive < cfg.RequiredSuccesses {
		return false
	}
	return cfg.StableFor == 0 || now.Sub(progress.stableSince) >= cfg.StableFor
}

func stabilityDetail(cfg Config, progress *stabilityProgress, now time.Time) string {
	if progress.consecutive < cfg.RequiredSuccesses {
		return "satisfied " + pluralCount(progress.consecutive, "success") + " of " + pluralCount(cfg.RequiredSuccesses, "success")
	}
	elapsed := now.Sub(progress.stableSince)
	return "stable for " + elapsed.Truncate(time.Millisecond).String() + " of " + cfg.StableFor.String()
}

func pluralCount(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "es"
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

type pollSchedule struct {
	base    time.Duration
	max     time.Duration
	backoff Backoff
	jitter  float64
	current time.Duration
}

func newPollSchedule(cfg Config) *pollSchedule {
	return &pollSchedule{
		base:    cfg.Interval,
		max:     cfg.MaxInterval,
		backoff: cfg.Backoff,
		jitter:  cfg.Jitter,
		current: cfg.Interval,
	}
}

func (s *pollSchedule) next(previousSatisfied bool) time.Duration {
	if previousSatisfied {
		s.current = s.base
		return s.withJitter(s.base)
	}
	interval := s.current
	if s.backoff == BackoffExponential {
		s.current = nextExponentialInterval(s.current, s.max)
	}
	return s.withJitter(interval)
}

func nextExponentialInterval(current, max time.Duration) time.Duration {
	if current >= max/2 {
		return max
	}
	return minDuration(current*2, max)
}

func (s *pollSchedule) withJitter(interval time.Duration) time.Duration {
	if s.jitter == 0 {
		return interval
	}
	factor := 1 - s.jitter + randomUnitFloat()*2*s.jitter
	jittered := time.Duration(float64(interval) * factor)
	return maxDuration(jittered, time.Nanosecond)
}

func randomUnitFloat() float64 {
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return 0.5
	}
	return float64(n.Int64()) / 1_000_000
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func runCondition(
	ctx context.Context,
	cond condition.Condition,
	gate conditionGate,
	cfg Config,
	start time.Time,
	record *ConditionResult,
	cancel context.CancelFunc,
	readyRemaining *atomic.Int64,
	terminal *atomic.Int32,
	attempts *attemptRecorder,
) {
	conditionStart := time.Now()
	progress := stabilityProgress{}
	schedule := newPollSchedule(cfg)
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

		rawResult, checked := checkCondition(ctx, cond, gate, cfg.PerAttemptTimeout)
		if !checked {
			return
		}

		attempt := record.Attempts + 1
		result := rawResult
		if record.Guard {
			progress.reset()
		} else {
			result = applyStabilityThreshold(result, cfg, &progress, time.Now())
		}

		done, shouldRecord := resultEndsCondition(result, cfg, record, cancel, readyRemaining, terminal)
		if shouldRecord {
			record.Attempts = attempt
			updateRecord(record, result, conditionStart, start)
			attempts.emit(buildAttemptEvent(record, attempt, result, start))
		}
		if done {
			return
		}

		interval := schedule.next(rawResult.Status == condition.CheckSatisfied)
		if !waitInterval(ctx, timer, interval) {
			return
		}
	}
}

func resultEndsCondition(
	result condition.Result,
	cfg Config,
	record *ConditionResult,
	cancel context.CancelFunc,
	readyRemaining *atomic.Int64,
	terminal *atomic.Int32,
) (bool, bool) {
	switch result.Status {
	case condition.CheckFatal:
		if terminal.CompareAndSwap(int32(terminalNone), int32(terminalFatal)) {
			cancel()
			return true, true
		}
		return true, false
	case condition.CheckSatisfied:
		return cancelSatisfiedRun(cfg, record, cancel, readyRemaining, terminal), true
	default:
		return false, true
	}
}

func cancelSatisfiedRun(
	cfg Config,
	record *ConditionResult,
	cancel context.CancelFunc,
	readyRemaining *atomic.Int64,
	terminal *atomic.Int32,
) bool {
	if cfg.Mode == ModeAny {
		if terminal.CompareAndSwap(int32(terminalNone), int32(terminalSatisfied)) {
			cancel()
			return true
		}
		return true
	}
	if !record.Guard && readyRemaining.Add(-1) == 0 {
		if terminal.CompareAndSwap(int32(terminalNone), int32(terminalSatisfied)) {
			cancel()
		}
	}
	return true
}

func outcomeSatisfied(records []ConditionResult, mode Mode) bool {
	if mode == ModeAny {
		for _, rec := range records {
			if !rec.Guard && rec.Satisfied {
				return true
			}
		}
		return false
	}
	for _, rec := range records {
		if !rec.Guard && !rec.Satisfied {
			return false
		}
	}
	return true
}
