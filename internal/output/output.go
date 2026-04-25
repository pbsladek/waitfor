package output

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

type Printer struct {
	w       io.Writer
	format  Format
	verbose bool
	mu      sync.Mutex
}

type Attempt struct {
	Name      string
	Attempt   int
	Satisfied bool
	Detail    string
	Error     string
	Elapsed   time.Duration
}

// Report is the stable output-facing run report serialized by JSON output.
type Report struct {
	Status                   string            `json:"status"`
	Satisfied                bool              `json:"satisfied"`
	Mode                     string            `json:"mode"`
	ElapsedSeconds           float64           `json:"elapsed_seconds"`
	TimeoutSeconds           float64           `json:"timeout_seconds"`
	IntervalSeconds          float64           `json:"interval_seconds"`
	PerAttemptTimeoutSeconds float64           `json:"per_attempt_timeout_seconds,omitempty"`
	RequiredSuccesses        int               `json:"required_successes,omitempty"`
	StableForSeconds         float64           `json:"stable_for_seconds,omitempty"`
	Conditions               []ConditionReport `json:"conditions"`
}

type ConditionReport struct {
	Backend        string  `json:"backend,omitempty"`
	Target         string  `json:"target,omitempty"`
	Name           string  `json:"name"`
	Satisfied      bool    `json:"satisfied"`
	Attempts       int     `json:"attempts"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	Detail         string  `json:"detail,omitempty"`
	LastError      string  `json:"last_error,omitempty"`
	Fatal          bool    `json:"fatal,omitempty"`
	Guard          bool    `json:"guard,omitempty"`
}

func NewPrinter(w io.Writer, format Format, verbose bool) *Printer {
	return &Printer{w: w, format: format, verbose: verbose}
}

func (p *Printer) Start(count int, timeout time.Duration, interval time.Duration) {
	if p.format != FormatText {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.w, "[waitfor] checking %d condition(s) (timeout: %s, interval: %s)\n", count, timeout, interval)
}

func (p *Printer) Attempt(event Attempt) {
	if p.format != FormatText {
		return
	}
	if !p.verbose && !event.Satisfied {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if event.Satisfied {
		_, _ = fmt.Fprintf(p.w, "[waitfor] [ok] %s (attempt %d, %.1fs) %s\n", event.Name, event.Attempt, event.Elapsed.Seconds(), event.Detail)
		return
	}
	if event.Error != "" {
		_, _ = fmt.Fprintf(p.w, "[waitfor] [..] %s (attempt %d) %s\n", event.Name, event.Attempt, event.Error)
		return
	}
	_, _ = fmt.Fprintf(p.w, "[waitfor] [..] %s (attempt %d) %s\n", event.Name, event.Attempt, event.Detail)
}

func (p *Printer) Outcome(report Report) error {
	if p.format == FormatJSON {
		return WriteJSON(p.w, report)
	}
	switch report.Status {
	case "satisfied":
		_, _ = fmt.Fprintf(p.w, "[waitfor] conditions satisfied in %.3fs\n", report.ElapsedSeconds)
		return nil
	case "fatal":
		_, _ = fmt.Fprintf(p.w, "[waitfor] stopped after %.3fs due to unrecoverable error\n", report.ElapsedSeconds)
	case "cancelled":
		_, _ = fmt.Fprintf(p.w, "[waitfor] cancelled after %.3fs\n", report.ElapsedSeconds)
	default:
		_, _ = fmt.Fprintf(p.w, "[waitfor] timeout after %.3fs\n", report.ElapsedSeconds)
	}
	p.printUnsatisfiedConditions(report.Conditions)
	return nil
}

func (p *Printer) printUnsatisfiedConditions(conditions []ConditionReport) {
	for _, rec := range conditions {
		if rec.Satisfied {
			continue
		}
		_, _ = fmt.Fprintf(p.w, "[waitfor] unsatisfied: %s%s\n", rec.Name, conditionIssue(rec))
	}
}

func conditionIssue(rec ConditionReport) string {
	if rec.LastError != "" {
		return ": " + rec.LastError
	}
	if rec.Detail != "" {
		return ": " + rec.Detail
	}
	return ""
}

func WriteJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func roundSeconds(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func Seconds(d time.Duration) float64 {
	return roundSeconds(d.Seconds())
}
