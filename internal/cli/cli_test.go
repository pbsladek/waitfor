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

	"github.com/pbsladek/wait-for/internal/condition"
)

func TestExecuteFileJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "json", "file", path, "exists"}, nil, &stdout, &stderr)
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

func TestExecuteTextWritesProgressToStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"file", path, "exists"}, nil, &stdout, &stderr)
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
	code := Execute(t.Context(), []string{"--timeout", "20ms", "--interval", "5ms", "file", path, "exists"}, nil, &stdout, &stderr)
	if code != ExitTimeout {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitTimeout, stdout.String(), stderr.String())
	}
}

func TestExecuteCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	path := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := Execute(ctx, []string{"--timeout", "1s", "--interval", "5ms", "file", path, "exists"}, nil, &stdout, &stderr)
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
		"file", path, "exists",
		"--", "file", missing, "exists",
	}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitSatisfied, stdout.String(), stderr.String())
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
		{name: "single", args: []string{"file", "README.md", "exists"}, want: 1},
		{name: "multiple", args: []string{"file", "README.md", "exists", "--", "tcp", "127.0.0.1:1"}, want: 2},
		{name: "bare separator inside exec command", args: []string{"exec", "--", "/bin/echo", "--", "not-a-backend"}, want: 1},
		{name: "exec command named backend", args: []string{"exec", "--", "http", "--version"}, want: 1},
		{name: "condition after exec command", args: []string{"exec", "--", "/bin/true", "--", "http", "http://example.com"}, want: 2},
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
		{name: "trailing separator", args: []string{"file", "README.md", "exists", "--"}},
		{name: "unknown backend", args: []string{"nope", "target"}},
		{name: "global flag after backend", args: []string{"file", "README.md", "exists", "--timeout", "1s"}},
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
	code := Execute(t.Context(), []string{"--timeout", "file", "README.md", "exists"}, nil, &stdout, &stderr)
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
	code := Execute(t.Context(), []string{"--output", "xml", "file", "x", "exists"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}

func TestExecuteGlobalInvalidMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--mode", "bogus", "file", "x", "exists"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stderr = %q", code, ExitInvalid, stderr.String())
	}
}
