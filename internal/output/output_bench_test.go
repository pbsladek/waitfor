package output

import (
	"io"
	"strconv"
	"testing"
	"time"
)

type slowWriter struct {
	delay time.Duration
}

func (w slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

func BenchmarkPrinterAttemptParallel(b *testing.B) {
	benchmarks := []struct {
		name string
		w    io.Writer
	}{
		{name: "discard", w: io.Discard},
		{name: "slow-writer-10us", w: slowWriter{delay: 10 * time.Microsecond}},
	}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			printer := NewPrinter(bm.w, FormatText, true)
			attempt := Attempt{Name: "http http://example.test/health", Attempt: 1, Detail: "status 503"}
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					printer.Attempt(attempt)
				}
			})
		})
	}
}

func BenchmarkWriteJSON(b *testing.B) {
	for _, count := range []int{1, 100, 1000, 10000} {
		b.Run("conditions="+strconv.Itoa(count), func(b *testing.B) {
			report := benchmarkReport(count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := WriteJSON(io.Discard, report); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkReport(count int) Report {
	conditions := make([]ConditionReport, count)
	for i := range conditions {
		conditions[i] = ConditionReport{
			Backend:        "http",
			Target:         "https://api.example.test/health/" + strconv.Itoa(i),
			Name:           "http https://api.example.test/health/" + strconv.Itoa(i),
			Satisfied:      i%2 == 0,
			Attempts:       3,
			ElapsedSeconds: 1.234,
			Detail:         "status 200",
			LastError:      "",
		}
	}
	return Report{
		Status:          "satisfied",
		Satisfied:       true,
		Mode:            "all",
		ElapsedSeconds:  1.5,
		TimeoutSeconds:  300,
		IntervalSeconds: 2,
		Conditions:      conditions,
	}
}
