package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/condition"
	"github.com/pbsladek/wait-for/internal/output"
	"github.com/pbsladek/wait-for/internal/runner"
)

func TestExecuteFileJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "json", "file", path, "--exists"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty for JSON output", stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if payload["satisfied"] != true {
		t.Fatalf("satisfied = %v, want true", payload["satisfied"])
	}
}

func TestExecuteConditionNameJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "json", "file", path, "--exists", "--name", "ready-file"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	var report output.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if got := report.Conditions[0].Name; got != "ready-file" {
		t.Fatalf("condition name = %q, want ready-file", got)
	}
}

func TestExecuteBackoffOptionsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--output", "json",
		"--interval", "10ms",
		"--backoff", "exponential",
		"--max-interval", "50ms",
		"--jitter", "20%",
		"file", path, "--exists",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	var report output.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if report.Backoff != "exponential" || report.MaxIntervalSeconds != 0.05 || report.Jitter != 0.2 {
		t.Fatalf("backoff report = %q/%v/%v", report.Backoff, report.MaxIntervalSeconds, report.Jitter)
	}
}

func TestExecuteDoctorJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"doctor", "--output", "json"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if report.Status == "" || len(report.Checks) == 0 {
		t.Fatalf("doctor report incomplete: %+v", report)
	}
}

func TestExecuteDoctorHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"doctor", "--help"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	if !strings.Contains(stdout.String(), "waitfor doctor") {
		t.Fatalf("stdout = %q, want doctor help", stdout.String())
	}
}

func TestRunDoctorText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"doctor"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitSatisfied, stderr.String())
	}
	if !strings.Contains(stdout.String(), "waitfor doctor") || !strings.Contains(stdout.String(), "temp") {
		t.Fatalf("stdout = %q, want text doctor report", stdout.String())
	}
}

func TestReportFromOutcomeIncludesCurrentBackendDetails(t *testing.T) {
	report := reportFromOutcome(runner.Outcome{
		Status:   runner.StatusTimeout,
		Mode:     runner.ModeAll,
		Elapsed:  100 * time.Millisecond,
		Timeout:  1 * time.Second,
		Interval: 10 * time.Millisecond,
		Conditions: []runner.ConditionResult{
			{Backend: "dns", Target: "example.com", Name: "dns example.com", Attempts: 2, Detail: "rcode NOERROR"},
			{Backend: "docker", Target: "api", Name: "docker api", Attempts: 1, LastError: "docker container not found: api"},
			{Backend: "k8s", Target: "pod/api", Name: "k8s pod/api", Attempts: 3, Detail: "condition Ready=False"},
		},
	})
	if len(report.Conditions) != 3 {
		t.Fatalf("len(conditions) = %d, want 3", len(report.Conditions))
	}
	assertConditionReport(t, report.Conditions[0], "dns", "example.com", "rcode NOERROR")
	assertConditionReport(t, report.Conditions[1], "docker", "api", "docker container not found: api")
	assertConditionReport(t, report.Conditions[2], "k8s", "pod/api", "condition Ready=False")
}

func assertConditionReport(t *testing.T, got output.ConditionReport, backend, target, detail string) {
	t.Helper()
	if got.Backend != backend {
		t.Fatalf("backend = %q, want %q", got.Backend, backend)
	}
	if got.Target != target {
		t.Fatalf("target = %q, want %q", got.Target, target)
	}
	if got.Detail != detail && got.LastError != detail {
		t.Fatalf("detail/last_error = %q/%q, want %q", got.Detail, got.LastError, detail)
	}
}

func TestExecuteTextWritesProgressToStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"file", path, "--exists"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty for text output", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want text progress")
	}
}

func TestExecuteTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--timeout", "20ms", "--interval", "5ms", "file", path, "--exists"}, nil, &stdout, &stderr)
	if code != ExitTimeout {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitTimeout, stdout.String(), stderr.String())
	}
}

func TestExecuteCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	path := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := Execute(ctx, []string{"--timeout", "1s", "--interval", "5ms", "file", path, "--exists"}, nil, &stdout, &stderr)
	if code != ExitCancelled {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitCancelled, stdout.String(), stderr.String())
	}
}

func TestExecuteHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Smoke"); got != "yes" {
			t.Fatalf("X-Smoke = %q, want yes", got)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "ping" {
			t.Fatalf("body = %q, want ping", string(body))
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"ready":true,"message":"ok"}`)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--output", "json",
		"http", server.URL,
		"--method", "POST",
		"--status", "2xx",
		"--body", "ping",
		"--body-contains", "ok",
		"--body-matches", `"message":"ok"`,
		"--jsonpath", ".ready == true",
		"--header", "X-Smoke=yes",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteHTTPBodyFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "from-file" {
			t.Fatalf("body = %q, want from-file", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"http", server.URL,
		"--method", "POST",
		"--body-file", bodyPath,
		"--body-contains", "ok",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"tcp", listener.Addr().String()}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
	<-accepted
}

func TestExecuteModeAnyWithMultipleConditions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--timeout", "100ms",
		"--interval", "5ms",
		"--mode", "any",
		"file", path, "--exists",
		"--", "file", missing, "--exists",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteGuardConditionFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fatal.log")
	if err := os.WriteFile(path, []byte("FATAL startup failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--timeout", "200ms",
		"--interval", "5ms",
		"file", filepath.Join(t.TempDir(), "missing"), "--exists",
		"--", "guard", "log", path, "--matches", "FATAL", "--from-start",
	}, nil, &stdout, &stderr)
	if code != ExitFatal {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitFatal, stdout.String(), stderr.String())
	}
}

func TestExecuteStableSuccesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--successes", "2",
		"--interval", "1ms",
		"file", path, "--exists",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteStableSuccessesJSONClearsLastError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--output", "json",
		"--successes", "2",
		"--interval", "1ms",
		"file", path, "--exists",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
	var report output.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if got := report.Conditions[0].LastError; got != "" {
		t.Fatalf("last_error = %q, want empty after final success", got)
	}
}

func TestExecuteExecRequiresFlagsBeforeSeparator(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"exec", "--output-contains", "ready", "--", "/bin/sh", "-c", "printf ready",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteExecCwdEnvAndOutputLimit(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"exec",
		"--cwd", dir,
		"--env", "WAITFOR_TEST=yes",
		"--max-output-bytes", fmt.Sprint(len(dir) + len(":yes")),
		"--output-contains", ":yes",
		"--", "/bin/sh", "-c", "printf '%s:%s:long-output' \"$PWD\" \"$WAITFOR_TEST\"",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
}

func TestExecuteExecCommandHelpDoesNotTriggerWaitforHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"exec", "--output-contains", "usage", "--", "/bin/sh", "-c", "printf usage --help",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "semantic condition poller") || strings.Contains(stderr.String(), "semantic condition poller") {
		t.Fatalf("waitfor help was printed unexpectedly, stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestExecuteExecDoesNotParseFlagsAfterSeparator(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"--timeout", "20ms",
		"--interval", "5ms",
		"exec", "--", "/bin/sh", "-c", "exit 1", "--exit-code", "1",
	}, nil, &stdout, &stderr)
	if code != ExitTimeout {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitTimeout, stdout.String(), stderr.String())
	}
}

func TestExecuteInvalidArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"tcp", "not-a-port"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitInvalid, stdout.String(), stderr.String())
	}
}

func TestExecuteInvalidHTTPURLDoesNotEchoSensitiveInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"http", "https://user:pass@"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitInvalid, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "user") || strings.Contains(stderr.String(), "pass") {
		t.Fatalf("stderr = %q leaked sensitive URL input", stderr.String())
	}
}

func TestSplitConditionSegments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "single", args: []string{"file", "README.md", "--exists"}, want: 1},
		{name: "multiple", args: []string{"file", "README.md", "--exists", "--", "tcp", "127.0.0.1:1"}, want: 2},
		{name: "bare separator inside exec command", args: []string{"exec", "--", "/bin/echo", "--", "not-a-backend"}, want: 1},
		{name: "exec command named backend", args: []string{"exec", "--", "http", "--version"}, want: 1},
		{name: "condition after exec command", args: []string{"exec", "--", "/bin/true", "--", "http", "http://example.com"}, want: 2},
		{name: "literal separator flag value before backend token", args: []string{"file", "README.md", "--contains", "--", "http"}, want: 1},
		{name: "literal trailing separator flag value", args: []string{"file", "README.md", "--contains", "--"}, want: 1},
		{name: "guard condition", args: []string{"file", "README.md", "--exists", "--", "guard", "log", "app.log", "--contains", "panic"}, want: 2},
		{name: "literal guard in exec command", args: []string{"exec", "--", "/bin/echo", "--", "guard"}, want: 1},
		{name: "dns literal separator value before guard", args: []string{"dns", "example.com", "--contains", "--", "--", "guard", "log", "app.log", "--contains", "panic"}, want: 2},
		{name: "dns equals literal separator value before guard", args: []string{"dns", "example.com", "--equals", "--", "--", "guard", "log", "app.log", "--contains", "panic"}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitConditionSegments(tt.args)
			if err != nil {
				t.Fatalf("splitConditionSegments() error = %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("len(splitConditionSegments()) = %d, want %d: %#v", len(got), tt.want, got)
			}
		})
	}
}

func TestParseFileConditionFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantState condition.FileState
		wantErr   string
	}{
		{name: "default exists", args: []string{"file", "/tmp/f"}, wantState: condition.FileExists},
		{name: "explicit exists", args: []string{"file", "/tmp/f", "--exists"}, wantState: condition.FileExists},
		{name: "deleted", args: []string{"file", "/tmp/f", "--deleted"}, wantState: condition.FileDeleted},
		{name: "nonempty", args: []string{"file", "/tmp/f", "--nonempty"}, wantState: condition.FileNonEmpty},
		{name: "mutual exclusion", args: []string{"file", "/tmp/f", "--exists", "--deleted"}, wantErr: "mutually exclusive"},
		{name: "extra positional", args: []string{"file", "/tmp/f", "exists"}, wantErr: "exactly one PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := parseFileCondition(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFileCondition() error = %v", err)
			}
			fc, ok := cond.(*condition.FileCondition)
			if !ok {
				t.Fatalf("condition type = %T, want *condition.FileCondition", cond)
			}
			if fc.State != tt.wantState {
				t.Fatalf("State = %q, want %q", fc.State, tt.wantState)
			}
		})
	}
}

func TestParseExecConditionRejectsNegativeExitCode(t *testing.T) {
	_, err := parseExecCondition([]string{"exec", "--exit-code", "-1", "--", "/bin/sh", "-c", "exit 0"})
	if err == nil {
		t.Fatal("parseExecCondition() expected negative exit-code error, got nil")
	}
	if !strings.Contains(err.Error(), "--exit-code cannot be negative") {
		t.Fatalf("err = %q, want negative exit-code error", err)
	}
}

func TestParseExecConditionCommandNamedBackend(t *testing.T) {
	segments, err := splitConditionSegments([]string{"exec", "--", "http", "--version"})
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("len(segments) = %d, want 1: %#v", len(segments), segments)
	}
	cond, err := parseExecCondition(segments[0])
	if err != nil {
		t.Fatalf("parseExecCondition() error = %v", err)
	}
	execCond, ok := cond.(*condition.ExecCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.ExecCondition", cond)
	}
	if got := strings.Join(execCond.Command, " "); got != "http --version" {
		t.Fatalf("command = %q, want %q", got, "http --version")
	}
}

func TestExecuteParserEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "trailing separator", args: []string{"file", "README.md", "--exists", "--"}},
		{name: "unknown backend", args: []string{"nope", "target"}},
		{name: "global flag after backend", args: []string{"file", "README.md", "--exists", "--timeout", "1s"}},
		{name: "exec missing separator", args: []string{"exec", "/bin/echo", "ready"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), tt.args, nil, &stdout, &stderr)
			if code != ExitInvalid {
				t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitInvalid, stdout.String(), stderr.String())
			}
		})
	}
}

func TestExecuteMalformedGlobalFlagReportsFlagError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--timeout", "file", "README.md", "--exists"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitInvalid, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--timeout") {
		t.Fatalf("stderr = %q, want timeout flag error", stderr.String())
	}
}

// ── parseGlobal ───────────────────────────────────────────────────────────────

func TestParseGlobalErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"invalid output format", []string{"--output", "xml", "file", "x"}, "invalid output format"},
		{"invalid mode", []string{"--mode", "bogus", "file", "x"}, "invalid mode"},
		{"zero timeout", []string{"--timeout", "0s", "file", "x"}, "timeout must be positive"},
		{"zero interval", []string{"--interval", "0s", "file", "x"}, "interval must be positive"},
		{"negative attempt-timeout", []string{"--attempt-timeout=-1ns", "file", "x"}, "attempt-timeout cannot be negative"},
		{"zero successes", []string{"--successes", "0", "file", "x"}, "successes must be at least 1"},
		{"negative stable-for", []string{"--stable-for=-1ns", "file", "x"}, "stable-for cannot be negative"},
		{"bad backoff", []string{"--backoff", "linear", "file", "x"}, "invalid backoff"},
		{"max interval below interval", []string{"--interval", "10ms", "--max-interval", "1ms", "file", "x"}, "max-interval"},
		{"negative jitter", []string{"--jitter", "-1%", "file", "x"}, "jitter"},
		{"bad jitter", []string{"--jitter", "sometimes", "file", "x"}, "invalid jitter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseGlobal(tt.args)
			if err == nil {
				t.Fatalf("parseGlobal() expected error %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseGlobal() err = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseJitterFraction(t *testing.T) {
	got, err := parseJitter("0.25")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.25 {
		t.Fatalf("jitter = %v, want 0.25", got)
	}
}

func TestParseDoctorOptions(t *testing.T) {
	opts, err := parseDoctorOptions([]string{"--output", "json", "--require", "docker,k8s", "--require", "dns-wire"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.format != output.FormatJSON {
		t.Fatalf("format = %q, want json", opts.format)
	}
	for _, name := range []string{"temp", "docker", "k8s", "dns-wire"} {
		if !opts.required[name] {
			t.Fatalf("required[%s] = false, want true", name)
		}
	}
}

func TestParseDoctorOptionsErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bad output", args: []string{"--output", "xml"}},
		{name: "bad require", args: []string{"--require", "printer"}},
		{name: "positional", args: []string{"extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDoctorOptions(tt.args)
			if err == nil {
				t.Fatal("parseDoctorOptions() expected error, got nil")
			}
		})
	}
}

func TestDoctorStatusCombination(t *testing.T) {
	if got := combineDoctorStatus(doctorOK, doctorCheck{Status: doctorWarn}); got != doctorWarn {
		t.Fatalf("optional warning status = %s, want warn", got)
	}
	if got := combineDoctorStatus(doctorOK, doctorCheck{Status: doctorWarn, Required: true}); got != doctorFail {
		t.Fatalf("required warning status = %s, want fail", got)
	}
}

func TestDoctorTextHelpers(t *testing.T) {
	report := doctorReport{
		Status:  doctorWarn,
		Version: "test",
		Commit:  "abc123",
		GOOS:    "testos",
		GOARCH:  "testarch",
		Checks: []doctorCheck{
			{Name: "temp", Status: doctorOK, Detail: "writable"},
			{Name: "docker", Status: doctorWarn},
		},
	}
	var buf bytes.Buffer
	if err := writeDoctorReport(&buf, output.FormatText, report); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"waitfor doctor: warn", "commit: abc123", "[ok] temp: writable", "[warn] docker"} {
		if !strings.Contains(got, want) {
			t.Fatalf("doctor text %q missing %q", got, want)
		}
	}
}

func TestRunDoctorCommandError(t *testing.T) {
	_, err := runDoctorCommand("definitely-no-such-waitfor-doctor-command")
	if err == nil {
		t.Fatal("runDoctorCommand() expected error, got nil")
	}
}

func TestParseConditionName(t *testing.T) {
	cond, err := parseCondition([]string{"file", "/tmp/ready", "--exists", "--name", "ready-file"})
	if err != nil {
		t.Fatalf("parseCondition() error = %v", err)
	}
	if got := cond.Descriptor().DisplayName(); got != "ready-file" {
		t.Fatalf("display = %q, want ready-file", got)
	}
}

func TestParseConditionNameErrors(t *testing.T) {
	tests := [][]string{
		{"file", "/tmp/ready", "--name"},
		{"file", "/tmp/ready", "--name", ""},
		{"file", "/tmp/ready", "--name", "a", "--name", "b"},
	}
	for _, segment := range tests {
		if _, err := parseCondition(segment); err == nil {
			t.Fatalf("parseCondition(%v) expected error, got nil", segment)
		}
	}
}

func TestParseConditionNameDoesNotConsumeExecCommandFlag(t *testing.T) {
	cond, err := parseCondition([]string{"exec", "--", "/bin/echo", "--name", "literal"})
	if err != nil {
		t.Fatalf("parseCondition() error = %v", err)
	}
	execCond, ok := cond.(*condition.ExecCondition)
	if !ok {
		t.Fatalf("condition type = %T, want exec condition", cond)
	}
	if got := strings.Join(execCond.Command, " "); got != "/bin/echo --name literal" {
		t.Fatalf("command = %q, want literal --name command", got)
	}
}

func TestParseBodyContentRejectsOversizedBodyFile(t *testing.T) {
	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	body := bytes.Repeat([]byte("x"), maxHTTPBodyFileBytes+1)
	if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseBodyContent("", bodyPath)
	if err == nil {
		t.Fatal("parseBodyContent() expected oversized body file error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("parseBodyContent() err = %q, want size error", err)
	}
}

func TestParseBodyContentRejectsNonRegularBodyFile(t *testing.T) {
	_, err := parseBodyContent("", t.TempDir())
	if err == nil {
		t.Fatal("parseBodyContent() expected non-regular body file error, got nil")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("parseBodyContent() err = %q, want regular file error", err)
	}
}

func TestExecuteExecRejectsZeroMaxOutputBytes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{
		"exec", "--max-output-bytes", "0", "--", "/bin/sh", "-c", "printf ok",
	}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
	if !strings.Contains(stderr.String(), "max-output-bytes") {
		t.Fatalf("stderr = %q, want max-output-bytes error", stderr.String())
	}
}

func TestValidateEnvVarsDoesNotEchoSensitiveInput(t *testing.T) {
	err := validateEnvVars([]string{"super-secret-token"})
	if err == nil {
		t.Fatal("validateEnvVars() expected error, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("err = %q leaked invalid env input", err)
	}
}

// ── parseKubernetesCondition ──────────────────────────────────────────────────

func TestParseKubernetesConditionSuccess(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantNS  string
	}{
		{"default namespace", []string{"k8s", "pod/myapp"}, "default"},
		{"explicit namespace", []string{"k8s", "pod/myapp", "--namespace", "prod"}, "prod"},
		{"with condition flag", []string{"k8s", "deployment/api", "--condition", "Available"}, "default"},
		{"with for rollout", []string{"k8s", "deployment/api", "--for", "rollout"}, "default"},
		{"with selector", []string{"k8s", "pod", "--selector", "app=api", "--for", "ready", "--all"}, "default"},
		{"with kubeconfig flag", []string{"k8s", "pod/myapp", "--kubeconfig", "/tmp/kube"}, "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := parseKubernetesCondition(tt.segment)
			if err != nil {
				t.Fatalf("parseKubernetesCondition() error = %v", err)
			}
			if cond == nil {
				t.Fatal("parseKubernetesCondition() returned nil condition")
			}
		})
	}
}

func TestParseKubernetesConditionWithJSONPath(t *testing.T) {
	cond, err := parseKubernetesCondition([]string{"k8s", "pod/myapp", "--jsonpath", ".status.phase == Running"})
	if err != nil {
		t.Fatalf("parseKubernetesCondition() error = %v", err)
	}
	if cond == nil {
		t.Fatal("parseKubernetesCondition() returned nil condition")
	}
}

func TestParseKubernetesConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{"missing resource", []string{"k8s"}, "exactly one RESOURCE"},
		{"too many args", []string{"k8s", "pod/a", "extra"}, "exactly one RESOURCE"},
		{"mutual exclusion", []string{"k8s", "pod/a", "--condition", "Ready", "--jsonpath", ".x"}, "mutually exclusive"},
		{"for mutual exclusion", []string{"k8s", "pod/a", "--for", "ready", "--condition", "Ready"}, "mutually exclusive"},
		{"bad for", []string{"k8s", "pod/a", "--for", "missing"}, "invalid kubernetes --for"},
		{"selector without for", []string{"k8s", "pod", "--selector", "app=api"}, "--selector requires --for"},
		{"selector with name", []string{"k8s", "pod/a", "--selector", "app=api", "--for", "ready"}, "resource kind without /name"},
		{"invalid selector", []string{"k8s", "pod", "--selector", "app in (", "--for", "ready"}, "invalid kubernetes selector"},
		{"all without selector", []string{"k8s", "pod/a", "--for", "ready", "--all"}, "--all requires --selector"},
		{"ready wrong kind", []string{"k8s", "deployment/a", "--for", "ready"}, "not supported"},
		{"complete wrong kind", []string{"k8s", "pod/a", "--for", "complete"}, "not supported"},
		{"rollout wrong kind", []string{"k8s", "job/a", "--for", "rollout"}, "not supported"},
		{"bad jsonpath", []string{"k8s", "pod/a", "--jsonpath", "  "}, "required"},
		{"unknown flag", []string{"k8s", "pod/a", "--no-such-flag"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseKubernetesCondition(tt.segment)
			if err == nil {
				t.Fatal("parseKubernetesCondition() expected error, got nil")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseKubernetesCondition() err = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

// ── parseTCPCondition ─────────────────────────────────────────────────────────

func TestParseTCPConditionNoArgs(t *testing.T) {
	_, err := parseTCPCondition([]string{"tcp"})
	if err == nil {
		t.Fatal("parseTCPCondition() expected error for no args, got nil")
	}
}

// ── parseDNSCondition ─────────────────────────────────────────────────────────

func TestParseDNSConditionSuccess(t *testing.T) {
	tests := [][]string{
		{"dns", "example.com"},
		{"dns", "example.com", "--type", "AAAA"},
		{"dns", "example.com", "--type", "txt", "--contains", "ready"},
		{"dns", "example.com", "--equals", "192.0.2.10", "--equals", "192.0.2.11", "--min-count", "1"},
		{"dns", "example.com", "--absent"},
		{"dns", "example.com", "--server", "1.1.1.1"},
		{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--type", "MX", "--rcode", "NOERROR"},
		{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--absent", "--absent-mode", "nxdomain"},
		{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--transport", "tcp", "--edns0", "--udp-size", "1232"},
	}
	for _, segment := range tests {
		cond, err := parseDNSCondition(segment)
		if err != nil {
			t.Fatalf("parseDNSCondition(%v) error = %v", segment, err)
		}
		if cond == nil {
			t.Fatalf("parseDNSCondition(%v) returned nil", segment)
		}
	}
}

func TestParseDNSConditionRepeatableEquals(t *testing.T) {
	cond, err := parseDNSCondition([]string{"dns", "example.com", "--equals", "192.0.2.10", "--equals", "192.0.2.11"})
	if err != nil {
		t.Fatal(err)
	}
	dnsCond := cond.(*condition.DNSCondition)
	if got := strings.Join(dnsCond.Equals, ","); got != "192.0.2.10,192.0.2.11" {
		t.Fatalf("equals = %q, want both values", got)
	}
}

func TestParseDNSConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{"missing host", []string{"dns"}, "exactly one HOST"},
		{"too many args", []string{"dns", "example.com", "extra"}, "exactly one HOST"},
		{"bad type", []string{"dns", "example.com", "--type", "BOGUS"}, "invalid dns record type"},
		{"mx requires wire", []string{"dns", "example.com", "--type", "MX"}, "requires --resolver wire"},
		{"bad resolver", []string{"dns", "example.com", "--resolver", "raw"}, "invalid dns resolver"},
		{"bad min count", []string{"dns", "example.com", "--min-count", "-1"}, "min-count cannot be negative"},
		{"absent conflict", []string{"dns", "example.com", "--absent", "--contains", "ready"}, "--absent cannot be combined"},
		{"bad absent mode", []string{"dns", "example.com", "--absent-mode", "gone"}, "invalid dns absent-mode"},
		{"wire-only absent mode", []string{"dns", "example.com", "--absent-mode", "nxdomain"}, "--absent-mode requires"},
		{"bad transport", []string{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--transport", "quic"}, "invalid dns transport"},
		{"bad rcode", []string{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--rcode", "READY"}, "invalid dns rcode"},
		{"wire-only rcode", []string{"dns", "example.com", "--rcode", "NOERROR"}, "require --resolver wire"},
		{"bad udp size", []string{"dns", "example.com", "--resolver", "wire", "--server", "1.1.1.1", "--udp-size", "70000"}, "udp-size"},
		{"wire missing server", []string{"dns", "example.com", "--resolver", "wire"}, "--resolver wire requires --server"},
		{"bad server", []string{"dns", "example.com", "--server", "host:"}, "invalid dns server address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDNSCondition(tt.segment)
			if err == nil {
				t.Fatal("parseDNSCondition() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseDNSServer(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"1.1.1.1", "1.1.1.1:53"},
		{"1.1.1.1:5353", "1.1.1.1:5353"},
		{"::1", "[::1]:53"},
		{"[::1]", "[::1]:53"},
		{"[::1]:5353", "[::1]:5353"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDNSServer(tt.input)
			if err != nil {
				t.Fatalf("parseDNSServer(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("parseDNSServer(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ── parseDockerCondition ──────────────────────────────────────────────────────

func TestParseDockerConditionSuccess(t *testing.T) {
	tests := [][]string{
		{"docker", "api"},
		{"docker", "api", "--status", "any"},
		{"docker", "api", "--status", "running", "--health", "healthy"},
	}
	for _, segment := range tests {
		cond, err := parseDockerCondition(segment)
		if err != nil {
			t.Fatalf("parseDockerCondition(%v) error = %v", segment, err)
		}
		if cond == nil {
			t.Fatalf("parseDockerCondition(%v) returned nil", segment)
		}
	}
}

func TestParseDockerConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{"missing container", []string{"docker"}, "exactly one CONTAINER"},
		{"too many args", []string{"docker", "api", "extra"}, "exactly one CONTAINER"},
		{"bad status", []string{"docker", "api", "--status", "warm"}, "invalid docker status"},
		{"bad health", []string{"docker", "api", "--health", "warm"}, "invalid docker health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseDockerCondition(tt.segment)
			if err == nil {
				t.Fatal("parseDockerCondition() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

// ── splitConditionSegments ────────────────────────────────────────────────────

func TestSplitConditionSegmentsLeadingDash(t *testing.T) {
	_, err := splitConditionSegments([]string{"--", "http", "http://x"})
	if err == nil {
		t.Fatal("splitConditionSegments() expected error for leading --, got nil")
	}
}

// ── exitError ─────────────────────────────────────────────────────────────────

func TestExitErrorMethod(t *testing.T) {
	e := exitError{code: 2, err: fmt.Errorf("something went wrong")}
	if got := e.Error(); got != "something went wrong" {
		t.Fatalf("exitError.Error() = %q, want %q", got, "something went wrong")
	}
	nilErr := exitError{code: 1, err: nil}
	if got := nilErr.Error(); got != "" {
		t.Fatalf("exitError.Error() (nil err) = %q, want empty", got)
	}
}

// ── splitHeader ───────────────────────────────────────────────────────────────

func TestSplitHeaderEmptyKey(t *testing.T) {
	_, _, ok := splitHeader("=value")
	if ok {
		t.Fatal("splitHeader('=value') should return ok=false for empty key")
	}
}

func TestSplitHeaderNoSeparator(t *testing.T) {
	_, _, ok := splitHeader("plain-value-no-separator")
	if ok {
		t.Fatal("splitHeader without separator should return ok=false")
	}
}

// ── Execute k8s integration paths ────────────────────────────────────────────

func TestExecuteK8sMissingResource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"k8s"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}

func TestExecuteK8sBadJSONPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"k8s", "pod/myapp", "--jsonpath", "  "}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}

func TestExecuteGlobalInvalidOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "xml", "file", "x"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}

func TestExecuteGlobalInvalidMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--mode", "bogus", "file", "x"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}
