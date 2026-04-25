package runner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
)

type benchCondition struct {
	result condition.Result
}

func (c benchCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "bench"}
}

func (c benchCondition) Check(context.Context) condition.Result {
	return c.result
}

type benchBlockingCondition struct{}

func (c benchBlockingCondition) Descriptor() condition.Descriptor {
	return condition.Descriptor{Name: "blocking"}
}

func (c benchBlockingCondition) Check(ctx context.Context) condition.Result {
	<-ctx.Done()
	return condition.Unsatisfied("", ctx.Err())
}

func BenchmarkRunManyConditions(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 5000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			conditions := make([]condition.Condition, n)
			for i := range conditions {
				conditions[i] = benchCondition{result: condition.Satisfied("ready")}
			}
			cfg := Config{
				Conditions: conditions,
				Timeout:    time.Second,
				Interval:   time.Hour,
				Mode:       ModeAll,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if out, err := Run(context.Background(), cfg); err != nil || out.Status != StatusSatisfied {
					b.Fatalf("Run() status = %s, err = %v", out.Status, err)
				}
			}
		})
	}
}

func BenchmarkRunOnAttemptDelayModeAny(b *testing.B) {
	for _, delay := range []time.Duration{0, time.Millisecond, 10 * time.Millisecond} {
		b.Run(delay.String(), func(b *testing.B) {
			cfg := Config{
				Conditions: []condition.Condition{
					benchCondition{result: condition.Satisfied("ready")},
					benchBlockingCondition{},
				},
				Timeout:  time.Second,
				Interval: time.Hour,
				Mode:     ModeAny,
				OnAttempt: func(AttemptEvent) {
					if delay > 0 {
						time.Sleep(delay)
					}
				},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if out, err := Run(context.Background(), cfg); err != nil || out.Status != StatusSatisfied {
					b.Fatalf("Run() status = %s, err = %v", out.Status, err)
				}
			}
		})
	}
}

func BenchmarkRunPerAttemptTimeout(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		for _, attemptTimeout := range []time.Duration{0, time.Second} {
			name := fmt.Sprintf("N=%d/attempt-timeout=%s", n, attemptTimeout)
			b.Run(name, func(b *testing.B) {
				conditions := make([]condition.Condition, n)
				for i := range conditions {
					conditions[i] = benchCondition{result: condition.Satisfied("ready")}
				}
				cfg := Config{
					Conditions:        conditions,
					Timeout:           time.Second,
					Interval:          time.Nanosecond,
					PerAttemptTimeout: attemptTimeout,
					RequiredSuccesses: 5,
					Mode:              ModeAll,
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if out, err := Run(context.Background(), cfg); err != nil || out.Status != StatusSatisfied {
						b.Fatalf("Run() status = %s, err = %v", out.Status, err)
					}
				}
			})
		}
	}
}
