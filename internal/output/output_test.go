package output

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSON(&buf, Report{
		Status:          "satisfied",
		Satisfied:       true,
		Mode:            "all",
		ElapsedSeconds:  1.5,
		TimeoutSeconds:  60,
		IntervalSeconds: 1,
		Conditions: []ConditionReport{
			{Backend: "tcp", Target: "localhost:5432", Name: "tcp localhost:5432", Satisfied: true, Attempts: 2, ElapsedSeconds: 1.4, Detail: "connection established"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "status": "satisfied",
  "satisfied": true,
  "mode": "all",
  "elapsed_seconds": 1.5,
  "timeout_seconds": 60,
  "interval_seconds": 1,
  "conditions": [
    {
      "backend": "tcp",
      "target": "localhost:5432",
      "name": "tcp localhost:5432",
      "satisfied": true,
      "attempts": 2,
      "elapsed_seconds": 1.4,
      "detail": "connection established"
    }
  ]
}
`
	if got := buf.String(); got != want {
		t.Fatalf("json output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteJSONStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{name: "timeout", status: "timeout", want: `"status": "timeout"`},
		{name: "cancelled", status: "cancelled", want: `"status": "cancelled"`},
		{name: "fatal", status: "fatal", want: `"status": "fatal"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := WriteJSON(&buf, Report{
				Status:          tt.status,
				Mode:            "any",
				ElapsedSeconds:  2,
				TimeoutSeconds:  5,
				IntervalSeconds: 1,
				Conditions: []ConditionReport{
					{Name: "condition", Attempts: 1, ElapsedSeconds: 1, LastError: "not ready", Fatal: tt.status == "fatal"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("output %q does not contain %q", buf.String(), tt.want)
			}
			if !strings.Contains(buf.String(), `"mode": "any"`) {
				t.Fatalf("output %q does not contain mode", buf.String())
			}
		})
	}
}

func TestPrinterText(t *testing.T) {
	var buf bytes.Buffer
	printer := NewPrinter(&buf, FormatText, true)
	printer.Start(1, time.Second, time.Millisecond)
	printer.Attempt(Attempt{Name: "file /tmp/ready exists", Attempt: 1, Satisfied: true, Detail: "exists"})
	if err := printer.Outcome(Report{Status: "satisfied", Satisfied: true}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"checking 1 condition", "[ok] file /tmp/ready exists", "conditions satisfied"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q does not contain %q", got, want)
		}
	}
}

func TestPrinterTextOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		report Report
		want   string
	}{
		{
			name:   "timeout",
			report: Report{Status: "timeout", ElapsedSeconds: 1.5, Conditions: []ConditionReport{{Name: "tcp x", LastError: "refused"}}},
			want:   "timeout",
		},
		{
			name:   "fatal",
			report: Report{Status: "fatal", ElapsedSeconds: 0.1, Conditions: []ConditionReport{{Name: "exec foo", LastError: "not found", Fatal: true}}},
			want:   "unrecoverable error",
		},
		{
			name:   "cancelled",
			report: Report{Status: "cancelled", ElapsedSeconds: 0.5, Conditions: []ConditionReport{{Name: "http x"}}},
			want:   "cancelled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := NewPrinter(&buf, FormatText, false)
			if err := p.Outcome(tt.report); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("output %q does not contain %q", buf.String(), tt.want)
			}
		})
	}
}

func TestPrinterTextUnsatisfiedConditionsListed(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatText, false)
	if err := p.Outcome(Report{
		Status:         "timeout",
		ElapsedSeconds: 5,
		Conditions: []ConditionReport{
			{Name: "http svc", Satisfied: false, LastError: "connection refused"},
			{Name: "tcp db", Satisfied: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "http svc") {
		t.Fatalf("output %q missing unsatisfied condition name", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("output %q missing last error", got)
	}
	if strings.Contains(got, "tcp db") {
		t.Fatalf("output %q should not list satisfied condition", got)
	}
}

func TestPrinterTextUnsatisfiedNoError(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatText, false)
	if err := p.Outcome(Report{
		Status:         "timeout",
		ElapsedSeconds: 1,
		Conditions: []ConditionReport{
			{Name: "file /tmp/f", Satisfied: false, Detail: "file is empty"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "file /tmp/f") {
		t.Fatalf("output %q missing unsatisfied condition name", buf.String())
	}
	if !strings.Contains(buf.String(), "file is empty") {
		t.Fatalf("output %q missing unsatisfied condition detail", buf.String())
	}
}

func TestPrinterTextAttemptUnsatisfiedWithError(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatText, true)
	p.Attempt(Attempt{Name: "tcp db", Attempt: 2, Satisfied: false, Error: "connection refused"})
	got := buf.String()
	if !strings.Contains(got, "tcp db") {
		t.Fatalf("verbose unsatisfied attempt %q missing name", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("verbose unsatisfied attempt %q missing error", got)
	}
}

func TestPrinterTextAttemptUnsatisfiedNonVerboseSuppressed(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatText, false)
	p.Attempt(Attempt{Name: "tcp db", Attempt: 1, Satisfied: false, Error: "refused"})
	if buf.Len() != 0 {
		t.Fatalf("non-verbose unsatisfied attempt should produce no output, got %q", buf.String())
	}
}

func TestPrinterTextAttemptUnsatisfiedDetailOnly(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatText, true)
	p.Attempt(Attempt{Name: "tcp db", Attempt: 1, Satisfied: false, Detail: "not ready"})
	got := buf.String()
	if !strings.Contains(got, "not ready") {
		t.Fatalf("verbose attempt %q missing detail", got)
	}
}

func TestPrinterJSONOutcomeWritesJSON(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrinter(&buf, FormatJSON, false)
	p.Start(1, time.Second, time.Second)
	p.Attempt(Attempt{Name: "tcp x", Attempt: 1, Satisfied: true})
	if err := p.Outcome(Report{Status: "satisfied", Satisfied: true, Mode: "all", Conditions: []ConditionReport{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"status": "satisfied"`) {
		t.Fatalf("json outcome %q missing status", buf.String())
	}
}

func TestSeconds(t *testing.T) {
	if got := Seconds(1500 * time.Millisecond); got != 1.5 {
		t.Fatalf("Seconds(1500ms) = %v, want 1.5", got)
	}
}
