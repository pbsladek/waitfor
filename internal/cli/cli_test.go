package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
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

func TestExecuteExplainJSONDoesNotRunCondition(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--explain", "--output", "json", "http", "https://example.test/health", "--bearer-token", "secret-token"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "secret-token") {
		t.Fatalf("explain leaked bearer token: %s", stdout.String())
	}
	var payload struct {
		Conditions []struct {
			Backend string `json:"backend"`
			Target  string `json:"target"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid explain JSON: %v", err)
	}
	if len(payload.Conditions) != 1 || payload.Conditions[0].Backend != "http" {
		t.Fatalf("conditions = %+v", payload.Conditions)
	}
}

func TestExecuteNDJSONOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--output", "ndjson", "file", path, "--exists"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("ndjson lines = %q", stdout.String())
	}
	var first map[string]any
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("last line JSON: %v", err)
	}
	if first["event"] != "start" || last["event"] != "outcome" {
		t.Fatalf("events first=%v last=%v", first["event"], last["event"])
	}
}

func TestExecuteConfigRecipeExplain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waitfor.yaml")
	body := []byte(`
timeout: 10m
mode: all
conditions:
  - name: api
    http:
      url: https://api.example.com/health
      status: 200
  - name: rollout
    k8s:
      resource: deployment/api
      for: rollout
guards:
  - log:
      path: /var/log/app.log
      matches: "FATAL|panic"
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--config", path, "--explain", "--output", "json"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var payload struct {
		TimeoutSeconds float64 `json:"timeout_seconds"`
		Conditions     []struct {
			Name  string `json:"name"`
			Guard bool   `json:"guard"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("recipe explain JSON: %v", err)
	}
	if payload.TimeoutSeconds != 600 || len(payload.Conditions) != 3 || !payload.Conditions[2].Guard {
		t.Fatalf("recipe explain = %+v", payload)
	}
}

func TestHelpCompletionProfileAndDoctorBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Execute(t.Context(), []string{"help", "http"}, nil, &stdout, &stderr); code != ExitSatisfied {
		t.Fatalf("help code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--bearer-token") {
		t.Fatalf("http help = %q", stdout.String())
	}
	stdout.Reset()
	if code := Execute(t.Context(), []string{"completion", "fish"}, nil, &stdout, &stderr); code != ExitSatisfied {
		t.Fatalf("completion code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "complete -c waitfor") {
		t.Fatalf("completion = %q", stdout.String())
	}
	stdout.Reset()
	if code := Execute(t.Context(), []string{"--profile", "ci", "--explain", "file", "/tmp/ready", "--exists"}, nil, &stdout, &stderr); code != ExitSatisfied {
		t.Fatalf("profile explain code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"timeout_seconds": 600`) {
		t.Fatalf("profile explain = %q", stdout.String())
	}
	stdout.Reset()
	if code := Execute(t.Context(), []string{"doctor", "--backend", "icmp", "--output", "json"}, nil, &stdout, &stderr); code != ExitSatisfied {
		t.Fatalf("doctor code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "backend:icmp") {
		t.Fatalf("doctor backend output = %q", stdout.String())
	}
}

func TestCompletionHelpers(t *testing.T) {
	tests := []struct {
		shell string
		want  string
	}{
		{"bash", "complete -W"},
		{"zsh", "#compdef waitfor"},
		{"fish", "complete -c waitfor"},
	}
	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			var stdout bytes.Buffer
			if err := runCompletion([]string{tt.shell}, &stdout); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), tt.want) || !strings.Contains(stdout.String(), "http") {
				t.Fatalf("completion = %q", stdout.String())
			}
		})
	}
	if err := runCompletion(nil, io.Discard); err == nil {
		t.Fatal("completion without shell succeeded")
	}
	if err := runCompletion([]string{"powershell"}, io.Discard); err == nil {
		t.Fatal("unsupported shell succeeded")
	}
	names := backendNames()
	if len(names) == 0 || names[0] > names[len(names)-1] || !containsString(names, "http") {
		t.Fatalf("backend names = %v", names)
	}
}

func TestExplainTextNDJSONAndLocalProfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--profile", "local", "--explain", "--output", "text", "file", "/tmp/ready", "--exists"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("text explain code = %d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "profile: local") || !strings.Contains(stdout.String(), "timeout: 120.000s") {
		t.Fatalf("text explain = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Execute(t.Context(), []string{"--explain", "--output", "ndjson", "file", "/tmp/ready", "--exists"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("ndjson explain code = %d stderr=%q", code, stderr.String())
	}
	var event struct {
		Event string `json:"event"`
		Plan  struct {
			Conditions []struct {
				Backend string `json:"backend"`
			} `json:"conditions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
		t.Fatalf("ndjson explain JSON: %v", err)
	}
	if event.Event != "explain" || len(event.Plan.Conditions) != 1 || event.Plan.Conditions[0].Backend != "file" {
		t.Fatalf("ndjson explain = %+v", event)
	}
}

func TestConfigRecipeFormsAndOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	body := []byte(`
timeout: 5m
interval: 2s
max_interval: 20s
backoff: exponential
jitter: 10%
attempt_timeout: 3s
successes: 2
stable_for: 4s
output: ndjson
mode: any
verbose: true
conditions:
  - args: ["file", "/tmp/ready", "--exists"]
  - name: api
    http:
      url: https://example.test/health
      header: ["X-One=1", "X-Two=2"]
      no_follow_redirects: true
  - exec:
      command: ["sh", "-c", "true"]
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--config", path, "--explain", "--output", "json", "--timeout", "1m"}, nil, &stdout, &stderr)
	if code != ExitSatisfied {
		t.Fatalf("config explain code = %d stderr=%q", code, stderr.String())
	}
	var plan struct {
		TimeoutSeconds           float64 `json:"timeout_seconds"`
		IntervalSeconds          float64 `json:"interval_seconds"`
		MaxIntervalSeconds       float64 `json:"max_interval_seconds"`
		Backoff                  string  `json:"backoff"`
		Jitter                   float64 `json:"jitter"`
		PerAttemptTimeoutSeconds float64 `json:"per_attempt_timeout_seconds"`
		RequiredSuccesses        int     `json:"required_successes"`
		StableForSeconds         float64 `json:"stable_for_seconds"`
		Mode                     string  `json:"mode"`
		Conditions               []struct {
			Backend string `json:"backend"`
			Name    string `json:"name"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatalf("config explain JSON: %v", err)
	}
	if plan.TimeoutSeconds != 60 || plan.IntervalSeconds != 2 || plan.MaxIntervalSeconds != 20 || plan.Backoff != "exponential" || plan.Jitter != 0.1 || plan.PerAttemptTimeoutSeconds != 3 || plan.RequiredSuccesses != 2 || plan.StableForSeconds != 4 || plan.Mode != "any" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Conditions) != 3 || plan.Conditions[1].Name != "api" || plan.Conditions[2].Backend != "exec" {
		t.Fatalf("conditions = %+v", plan.Conditions)
	}
}

func TestRecipeValidationErrors(t *testing.T) {
	tests := []string{
		"unknown_option: true\nconditions:\n  - file: /tmp/ready\n",
		"successes: 0\nconditions:\n  - file: /tmp/ready\n",
		"successes: -2\nconditions:\n  - file: /tmp/ready\n",
		"conditions:\n  - file: /tmp/ready\n    tcp: localhost:80\n",
		"conditions:\n  - args: [file, /tmp/ready]\n    file: /tmp/other\n",
		"conditions:\n  - args: [file, /tmp/ready]\n    typo: true\n",
		"conditions:\n  - file: /tmp/ready\n---\nconditions:\n  - file: /tmp/other\n",
		"output: xml\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"mode: one\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"timeout: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"interval: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"max_interval: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"jitter: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"attempt_timeout: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"stable_for: nope\nconditions:\n  - args: [file, /tmp/ready, --exists]\n",
		"conditions:\n  - args: [file, /tmp/ready, --exists]\nguards:\n  - args: [file, {bad: value}]\n",
		"conditions:\n  - args: [file, {bad: value}]\n",
		"conditions:\n  - exec:\n      command: nope\n",
		"conditions:\n  - http: [bad]\n",
		"conditions:\n  - name: empty\n",
		"conditions:\n  - file:\n      path: {bad: value}\n",
		"conditions:\n  - args: [file, /tmp/ready, --exists]\n      guard: true\n",
		"conditions: []\n",
	}
	for i, body := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := Execute(t.Context(), []string{"--config", path, "--explain"}, nil, &stdout, &stderr); code != ExitInvalid {
				t.Fatalf("code = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRecipeSuccessesCLIOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	if err := os.WriteFile(path, []byte("successes: 0\nconditions:\n  - file: /tmp/ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"--config", path, "--successes", "2", "--explain", "--output", "json"}, nil, &stdout, &stderr)
	if code != ExitSatisfied || !strings.Contains(stdout.String(), `"required_successes": 2`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestExecuteOutputFailure(t *testing.T) {
	for _, format := range []string{"text", "ndjson", "json"} {
		t.Run(format, func(t *testing.T) {
			var stderr bytes.Buffer
			stdout := io.Writer(errWriter{})
			errorOutput := io.Writer(&stderr)
			if format == "text" {
				errorOutput = errWriter{}
			}
			code := Execute(t.Context(), []string{"--output", format, "file", t.TempDir(), "--exists"}, nil, stdout, errorOutput)
			if code != ExitFatal {
				t.Fatalf("code=%d, want fatal", code)
			}
		})
	}
}

func TestExecuteCancelsAfterProgressOutputFailure(t *testing.T) {
	for _, format := range []string{"text", "ndjson"} {
		t.Run(format, func(t *testing.T) {
			writer := &progressErrorWriter{}
			stdout, stderr := io.Writer(io.Discard), io.Writer(io.Discard)
			if format == "text" {
				stderr = writer
			} else {
				stdout = writer
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			code := Execute(ctx, []string{"--timeout", "30s", "--output", format, "--verbose", "file", filepath.Join(t.TempDir(), "missing"), "--exists"}, nil, stdout, stderr)
			if code != ExitFatal || ctx.Err() != nil {
				t.Fatalf("code=%d context=%v, want prompt fatal output error", code, ctx.Err())
			}
		})
	}
}

type progressErrorWriter struct{ calls int }

func (w *progressErrorWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.calls == 2 {
		return 0, fmt.Errorf("progress output failed")
	}
	return len(data), nil
}

func TestRecipeHelperBranches(t *testing.T) {
	opts := globalOptions{
		timeout:           time.Minute,
		interval:          time.Second,
		maxInterval:       time.Second,
		backoff:           runner.BackoffConstant,
		requiredSuccesses: 1,
		format:            output.FormatText,
		mode:              runner.ModeAll,
		changed: map[string]bool{
			"timeout":         true,
			"attempt-timeout": true,
			"successes":       true,
			"stable-for":      true,
			"output":          true,
			"mode":            true,
			"verbose":         true,
		},
	}
	verbose := true
	successes := 3
	applied, err := applyRecipeOptions(opts, recipeFile{
		Timeout:        "1s",
		AttemptTimeout: "2s",
		Successes:      &successes,
		StableFor:      "4s",
		Output:         "json",
		Mode:           "any",
		Verbose:        &verbose,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.timeout != opts.timeout || applied.perAttemptTimeout != opts.perAttemptTimeout || applied.requiredSuccesses != opts.requiredSuccesses || applied.stableFor != opts.stableFor || applied.format != opts.format || applied.mode != opts.mode || applied.verbose != opts.verbose {
		t.Fatalf("changed recipe options were not preserved: before=%+v after=%+v", opts, applied)
	}

	if _, err := recipeSegments(recipeFile{Guards: []map[string]any{{"args": []any{"file", "/tmp/ready", "--exists"}}}}); err != nil {
		t.Fatalf("guard args segment error = %v", err)
	}
	segment, err := recipeBackendSegment("custom", map[string]any{"target": "value", "count": 2, "enabled": true, "skip": false, "list": []any{"a", 2, map[string]any{"bad": true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"custom", "value", "--count", "2", "--enabled", "--list", "a", "--list", "2"} {
		if !containsString(segment, want) {
			t.Fatalf("segment = %v, missing %q", segment, want)
		}
	}
	if got, ok := stringValue(42); ok || got != "" {
		t.Fatalf("stringValue(42) = %q, %v; want empty false", got, ok)
	}
	if got := boolValue("true"); got {
		t.Fatalf("boolValue(string) = %v; want false", got)
	}
}

func TestBackendParsersRejectUnknownFlags(t *testing.T) {
	for backend, parser := range backendParsers {
		t.Run(backend, func(t *testing.T) {
			if _, err := parser([]string{backend, "--definitely-unknown"}); err == nil {
				t.Fatal("unknown flag succeeded")
			}
		})
	}
}

func TestParseConditionAdditionalBranches(t *testing.T) {
	if _, err := parseCondition(nil); err == nil {
		t.Fatal("empty condition succeeded")
	}
	if _, err := parseCondition([]string{"guard"}); err == nil {
		t.Fatal("bare guard succeeded")
	}
	if _, err := parseCondition([]string{"guard", "unknown"}); err == nil {
		t.Fatal("guard with bad backend succeeded")
	}
	if _, err := parseCondition([]string{"file", "--name="}); err == nil {
		t.Fatal("empty equals name succeeded")
	}
	if _, err := parseCondition([]string{"file", "--name", "one", "--name=two", "/tmp/ready"}); err == nil {
		t.Fatal("duplicate condition name succeeded")
	}
	cleaned, name, err := parseConditionName([]string{"exec", "--", "echo", "--name=kept"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "" || !containsString(cleaned, "--name=kept") {
		t.Fatalf("exec command name parsing cleaned=%v name=%q", cleaned, name)
	}
	if !conditionNameReservedForBackend([]string{"process", "--name", "agent"}) {
		t.Fatal("process --name was not reserved for backend parser")
	}
}

func TestCLIHTTPBranchHelpers(t *testing.T) {
	if err := validateHTTPURL("ftp://example.test"); err == nil {
		t.Fatal("ftp HTTP URL succeeded")
	}
	if _, err := parseBodyContent("inline", "body.txt"); err == nil {
		t.Fatal("body and body-file succeeded")
	}
	if _, err := parseBodyContent("", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing body file succeeded")
	}
	headerTests := [][]string{
		{"NoSeparator"},
		{"Bad Header=value"},
		{"X-Bad=\x01"},
		{"X-One=1", "x-one=2"},
	}
	for _, headers := range headerTests {
		if _, err := parseHTTPHeaders(headers); err == nil {
			t.Fatalf("parseHTTPHeaders(%q) succeeded", headers)
		}
	}
	if validHTTPHeaderName("") {
		t.Fatal("empty header name valid")
	}
	if !validHTTPHeaderValue("one\ttwo") {
		t.Fatal("tab header value invalid")
	}
	if _, _, err := compileHTTPBodyMatchers("[", ""); err == nil {
		t.Fatal("bad body regex succeeded")
	}
	if _, _, err := compileHTTPBodyMatchers("", " "); err == nil {
		t.Fatal("bad body jsonpath succeeded")
	}
	if _, err := buildHTTPCondition("https://example.test", httpConditionOptions{status: "bad"}); err == nil {
		t.Fatal("bad HTTP status succeeded")
	}
	if _, err := buildHTTPCondition("https://example.test", httpConditionOptions{status: "200", body: "inline", bodyFile: "file"}); err == nil {
		t.Fatal("bad HTTP body config succeeded")
	}
	if _, err := buildHTTPCondition("https://example.test", httpConditionOptions{status: "200", rawHeaders: []string{"Bad Header=value"}}); err == nil {
		t.Fatal("bad HTTP header config succeeded")
	}
	authHeaders := map[string]string{"Authorization": "Bearer existing"}
	if err := applyHTTPAuthHeaders(authHeaders, "token", "", ""); err == nil {
		t.Fatal("authorization header conflict succeeded")
	}
	if mode := httpAuthMode("token", "user", "pass"); mode != "conflict" {
		t.Fatalf("auth mode = %q, want conflict", mode)
	}
	if mode := httpAuthMode("", "", "pass"); mode != "incomplete-basic" {
		t.Fatalf("auth mode = %q, want incomplete-basic", mode)
	}
	if _, err := loadHTTPClientCertificate("cert.pem", ""); err == nil {
		t.Fatal("partial client certificate succeeded")
	}
	if _, err := loadHTTPClientCertificate(filepath.Join(t.TempDir(), "missing-cert.pem"), filepath.Join(t.TempDir(), "missing-key.pem")); err == nil {
		t.Fatal("missing client certificate succeeded")
	}
}

func TestCLILocalParserBranchHelpers(t *testing.T) {
	if _, _, err := parsePortRange("bad-start-2"); err == nil {
		t.Fatal("bad start port succeeded")
	}
	if _, _, err := parsePortRange("1-bad-end"); err == nil {
		t.Fatal("bad end port succeeded")
	}
	if _, err := parsePortsCondition([]string{"ports", "example.test", "--range", "2-1"}); err == nil {
		t.Fatal("invalid port range condition succeeded")
	}
	if _, err := parseDNSCondition([]string{"dns", "example.test", "--udp-size", "-1"}); err == nil {
		t.Fatal("negative DNS UDP size succeeded")
	}
	if _, err := parseLockfileCondition([]string{"lockfile", "/tmp/app.lock", "--older-than", "nope"}); err == nil {
		t.Fatal("bad lockfile older-than succeeded")
	}
	permission := condition.NewPermission("/tmp/ready")
	if err := applyPermissionOptions(permission, "bad", -1, -1, "", "", "file"); err == nil {
		t.Fatal("bad permission mode succeeded")
	}
	if err := applyPermissionOptions(permission, "", 1, -1, "1", "", "file"); err == nil {
		t.Fatal("uid/user conflict succeeded")
	}
	if err := applyPermissionOptions(permission, "", -1, -1, "name", "", "file"); err == nil {
		t.Fatal("non-numeric user succeeded")
	}
	if err := applyPermissionOptions(permission, "", -1, 1, "", "1", "file"); err == nil {
		t.Fatal("gid/group conflict succeeded")
	}
	if err := applyPermissionOptions(permission, "", -1, -1, "", "name", "file"); err == nil {
		t.Fatal("non-numeric group succeeded")
	}
	if _, err := parseNTPCondition([]string{"ntp", "time.example", "--max-offset", "nope"}); err == nil {
		t.Fatal("bad ntp max offset succeeded")
	}
	if _, err := parseNTPCondition([]string{"ntp", "time.example", "--timeout", "nope"}); err == nil {
		t.Fatal("bad ntp timeout succeeded")
	}
	if _, err := parseICMPCondition([]string{"icmp", "127.0.0.1", "--timeout", "nope"}); err == nil {
		t.Fatal("bad icmp timeout succeeded")
	}
	if _, err := parseGRPCCondition([]string{"grpc", "127.0.0.1:50051", "--timeout", "nope"}); err == nil {
		t.Fatal("bad grpc timeout succeeded")
	}
	if _, err := parseWebSocketCondition([]string{"websocket", "ws://example.test", "--timeout", "nope"}); err == nil {
		t.Fatal("bad websocket timeout succeeded")
	}
	if _, err := parseWebSocketCondition([]string{"websocket", "ws://example.test", "--read-timeout", "nope"}); err == nil {
		t.Fatal("bad websocket read timeout succeeded")
	}
}

func TestConditionDiagnosticsReasonsAndSuggestions(t *testing.T) {
	tests := []struct {
		name string
		rec  runner.ConditionResult
		want string
	}{
		{"docker missing", runner.ConditionResult{Backend: "docker", LastError: "No such container"}, "missing"},
		{"docker unhealthy", runner.ConditionResult{Backend: "docker", Detail: "health unhealthy"}, "unhealthy"},
		{"process missing", runner.ConditionResult{Backend: "process", LastError: "does not exist"}, "missing"},
		{"systemd unavailable", runner.ConditionResult{Backend: "systemd", LastError: "systemctl command not found"}, "tool_unavailable"},
		{"k8s auth", runner.ConditionResult{Backend: "k8s", LastError: "forbidden"}, "auth"},
		{"fatal", runner.ConditionResult{Backend: "exec", Fatal: true}, "fatal"},
		{"unsatisfied", runner.ConditionResult{Backend: "http"}, "unsatisfied"},
		{"satisfied", runner.ConditionResult{Backend: "http", Satisfied: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conditionReason(tt.rec); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}

	suggestions := []runner.ConditionResult{
		{Backend: "http"},
		{Backend: "k8s"},
		{Backend: "docker"},
		{Backend: "log"},
		{Backend: "tcp"},
		{Backend: "tcp", LastError: "connection refused"},
	}
	for _, rec := range suggestions {
		if got := conditionSuggestion(runner.StatusTimeout, rec); got == "" {
			t.Fatalf("empty suggestion for %+v", rec)
		}
	}
	if got := conditionSuggestion(runner.StatusSatisfied, runner.ConditionResult{Backend: "http"}); got != "" {
		t.Fatalf("satisfied suggestion = %q", got)
	}
	if got := conditionSuggestion(runner.StatusTimeout, runner.ConditionResult{Backend: "http", Satisfied: true}); got != "" {
		t.Fatalf("condition satisfied suggestion = %q", got)
	}
}

func TestLoadHTTPClientCertificate(t *testing.T) {
	certPEM, keyPEM := testClientCertificatePEM(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := loadHTTPClientCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatalf("cert = %+v", cert)
	}
	cond, err := parseHTTPCondition([]string{"http", "https://example.test/ready", "--client-cert", certPath, "--client-key", keyPath})
	if err != nil {
		t.Fatal(err)
	}
	if cond.(*condition.HTTPCondition).ClientCert == nil {
		t.Fatal("parsed HTTP condition missing client cert")
	}
}

func testClientCertificatePEM(t *testing.T) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "waitfor-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
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

func TestExecuteUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets are not supported: %v", err)
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
	code := Execute(t.Context(), []string{"unix", path}, nil, &stdout, &stderr)
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

func TestExecuteGuardOnlyInvalidDoesNotPrintProgress(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), []string{"guard", "file", filepath.Join(t.TempDir(), "missing"), "--exists"}, nil, &stdout, &stderr)
	if code != ExitInvalid {
		t.Fatalf("exit code = %d, want %d, stdout = %q, stderr = %q", code, ExitInvalid, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "[waitfor] checking") {
		t.Fatalf("stderr = %q, want validation error before progress starts", stderr.String())
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
		"--timeout", "100ms",
		"--interval", "5ms",
		"exec",
		"--cwd", dir,
		"--env", "WAITFOR_TEST=yes",
		"--max-output-bytes", fmt.Sprint(len(":yes")),
		"--output-contains", ":yes",
		"--", "/bin/sh", "-c", "test -d \"$PWD\" && test \"$WAITFOR_TEST\" = yes && printf ':yes:long-output'",
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

func TestParseHTTPConditionRejectsInvalidJSONPath(t *testing.T) {
	_, err := parseHTTPCondition([]string{"http", "http://example.com", "--jsonpath", "ready == true"})
	if err == nil {
		t.Fatal("parseHTTPCondition() expected invalid jsonpath error, got nil")
	}
	if !strings.Contains(err.Error(), "jsonpath must start") {
		t.Fatalf("err = %q, want jsonpath error", err)
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
		{name: "log tail value before backend token", args: []string{"log", "app.log", "--contains", "ready", "--tail", "http", "--", "file", "README.md"}, want: 2},
		{name: "log min matches value before backend token", args: []string{"log", "app.log", "--contains", "ready", "--min-matches", "http", "--", "file", "README.md"}, want: 2},
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

func TestConditionValueFlagsCoversBackendValueFlags(t *testing.T) {
	valueFlags := []string{
		"--method",
		"--status",
		"--header",
		"--body",
		"--body-file",
		"--body-contains",
		"--body-matches",
		"--jsonpath",
		"--type",
		"--resolver",
		"--contains",
		"--matches",
		"--exclude",
		"--tail",
		"--min-matches",
		"--equals",
		"--min-count",
		"--max-count",
		"--absent-mode",
		"--server",
		"--rcode",
		"--transport",
		"--udp-size",
		"--servername",
		"--valid-for",
		"--ca-file",
		"--banner-contains",
		"--user",
		"--password",
		"--host-key-sha256",
		"--metadata",
		"--range",
		"--endpoint-url",
		"--region",
		"--access-key-id",
		"--secret-access-key",
		"--session-token",
		"--health",
		"--pid",
		"--namespace",
		"--condition",
		"--for",
		"--selector",
		"--kubeconfig",
		"--exit-code",
		"--output-contains",
		"--cwd",
		"--env",
		"--max-output-bytes",
		"--name",
	}
	for _, flag := range valueFlags {
		if !conditionValueFlags[flag] {
			t.Fatalf("conditionValueFlags[%q] = false, want true", flag)
		}
	}
}

func TestParseGlobConditionFlags(t *testing.T) {
	cond, err := parseGlobCondition([]string{"glob", "/tmp/jobs/*.done", "--min-count", "5", "--max-count", "10"})
	if err != nil {
		t.Fatalf("parseGlobCondition() error = %v", err)
	}
	globCond, ok := cond.(*condition.GlobCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.GlobCondition", cond)
	}
	if globCond.Pattern != "/tmp/jobs/*.done" || globCond.MinCount != 5 || globCond.MaxCount != 10 || globCond.Absent {
		t.Fatalf("glob condition = %+v", globCond)
	}
}

func TestParseGlobConditionAbsent(t *testing.T) {
	cond, err := parseGlobCondition([]string{"glob", "/tmp/jobs/*.done", "--absent"})
	if err != nil {
		t.Fatalf("parseGlobCondition() error = %v", err)
	}
	globCond, ok := cond.(*condition.GlobCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.GlobCondition", cond)
	}
	if !globCond.Absent || globCond.MinCount != 0 {
		t.Fatalf("glob condition = %+v, want absent min-count 0", globCond)
	}
}

func TestParseGlobConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing pattern", segment: []string{"glob"}, wantErr: "exactly one"},
		{name: "negative min", segment: []string{"glob", "*.done", "--min-count", "-1"}, wantErr: "negative"},
		{name: "bad max", segment: []string{"glob", "*.done", "--max-count", "-2"}, wantErr: "less than -1"},
		{name: "min exceeds max", segment: []string{"glob", "*.done", "--min-count", "2", "--max-count", "1"}, wantErr: "exceed"},
		{name: "absent positive min", segment: []string{"glob", "*.done", "--absent", "--min-count", "1"}, wantErr: "absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseGlobCondition(tt.segment)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParsePortsConditionFlags(t *testing.T) {
	cond, err := parsePortsCondition([]string{"ports", "localhost", "--range", "8000-8010", "--any"})
	if err != nil {
		t.Fatalf("parsePortsCondition() error = %v", err)
	}
	portsCond, ok := cond.(*condition.PortsCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.PortsCondition", cond)
	}
	if portsCond.Host != "localhost" || portsCond.StartPort != 8000 || portsCond.EndPort != 8010 || portsCond.Mode != condition.PortsAny {
		t.Fatalf("ports condition = %+v", portsCond)
	}
}

func TestParsePortsConditionDefaultsAll(t *testing.T) {
	cond, err := parsePortsCondition([]string{"ports", "localhost", "--range", "8000-8001"})
	if err != nil {
		t.Fatalf("parsePortsCondition() error = %v", err)
	}
	portsCond, ok := cond.(*condition.PortsCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.PortsCondition", cond)
	}
	if portsCond.Mode != condition.PortsAll {
		t.Fatalf("mode = %q, want all", portsCond.Mode)
	}
}

func TestParsePortsConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing host", segment: []string{"ports", "--range", "8000-8001"}, wantErr: "exactly one HOST"},
		{name: "missing range", segment: []string{"ports", "localhost"}, wantErr: "requires --range"},
		{name: "bad range", segment: []string{"ports", "localhost", "--range", "8000"}, wantErr: "invalid ports range"},
		{name: "bad port", segment: []string{"ports", "localhost", "--range", "0-1"}, wantErr: "invalid ports range"},
		{name: "reversed", segment: []string{"ports", "localhost", "--range", "2-1"}, wantErr: "invalid ports range"},
		{name: "mode conflict", segment: []string{"ports", "localhost", "--range", "1-2", "--any", "--all"}, wantErr: "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePortsCondition(tt.segment)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseSSHConditionFlags(t *testing.T) {
	cond, err := parseSSHCondition([]string{
		"ssh", "example.test:22",
		"--banner-contains", "OpenSSH",
		"--user", "deploy",
		"--password", "secret",
		"--host-key-sha256", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatalf("parseSSHCondition() error = %v", err)
	}
	sshCond, ok := cond.(*condition.SSHCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.SSHCondition", cond)
	}
	if sshCond.Address != "example.test:22" ||
		sshCond.BannerContains != "OpenSSH" ||
		sshCond.User != "deploy" ||
		sshCond.Password != "secret" ||
		sshCond.HostKeySHA256 != "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("ssh condition = %+v", sshCond)
	}
}

func TestParseSSHConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing address", segment: []string{"ssh"}, wantErr: "exactly one"},
		{name: "bad address", segment: []string{"ssh", "example.test"}, wantErr: "invalid ssh address"},
		{name: "partial auth user", segment: []string{"ssh", "example.test:22", "--user", "deploy"}, wantErr: "provided together"},
		{name: "partial auth password", segment: []string{"ssh", "example.test:22", "--password", "secret"}, wantErr: "provided together"},
		{name: "auth without host key", segment: []string{"ssh", "example.test:22", "--user", "deploy", "--password", "secret"}, wantErr: "host-key-sha256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSSHCondition(tt.segment)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseS3ConditionFlags(t *testing.T) {
	cond, err := parseS3Condition([]string{
		"s3", "s3://ready-bucket/path/ready.json",
		"--exists",
		"--metadata", "version=42",
		"--contains", `"ready":true`,
		"--endpoint-url", "https://127.0.0.1:9000",
		"--region", "auto",
		"--virtual-hosted-style",
		"--access-key-id", "test-access-key",
		"--secret-access-key", "test-secret-key",
		"--session-token", "test-session-token",
	})
	if err != nil {
		t.Fatalf("parseS3Condition() error = %v", err)
	}
	s3Cond, ok := cond.(*condition.S3Condition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.S3Condition", cond)
	}
	if s3Cond.URL != "s3://ready-bucket/path/ready.json" || s3Cond.EndpointURL != "https://127.0.0.1:9000" {
		t.Fatalf("s3 condition = %+v", s3Cond)
	}
	if !s3Cond.VirtualHostedStyle || s3Cond.Region != "auto" {
		t.Fatalf("s3 condition style/region = %+v", s3Cond)
	}
	if s3Cond.Metadata["version"] != "42" || s3Cond.Contains != `"ready":true` {
		t.Fatalf("s3 metadata/contains = %+v/%q", s3Cond.Metadata, s3Cond.Contains)
	}
	if s3Cond.Credentials.AccessKeyID != "test-access-key" ||
		s3Cond.Credentials.SecretAccessKey != "test-secret-key" ||
		s3Cond.Credentials.SessionToken != "test-session-token" {
		t.Fatalf("s3 credentials = %+v", s3Cond.Credentials)
	}
}

func TestParseS3ConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing url", segment: []string{"s3"}, wantErr: "exactly one"},
		{name: "bad url", segment: []string{"s3", "https://example.test/object"}, wantErr: "invalid s3 URL"},
		{name: "contains without key", segment: []string{"s3", "s3://bucket", "--contains", "ready"}, wantErr: "object key"},
		{name: "metadata without key", segment: []string{"s3", "s3://bucket", "--metadata", "version=1"}, wantErr: "object key"},
		{name: "bad metadata", segment: []string{"s3", "s3://bucket/key", "--metadata", "version"}, wantErr: "Key=Value"},
		{name: "bad endpoint", segment: []string{"s3", "s3://bucket/key", "--endpoint-url", "ftp://example.test"}, wantErr: "http or https"},
		{name: "endpoint userinfo", segment: []string{"s3", "s3://bucket/key", "--endpoint-url", "https://user@example.test"}, wantErr: "userinfo"},
		{name: "plaintext credentials", segment: []string{"s3", "s3://bucket/key", "--endpoint-url", "http://127.0.0.1:9000", "--access-key-id", "id", "--secret-access-key", "secret"}, wantErr: "https"},
		{name: "blank region", segment: []string{"s3", "s3://bucket/key", "--region", " "}, wantErr: "region"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseS3Condition(tt.segment)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultS3EndpointURL(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL", "https://generic.example.test")
	t.Setenv("S3_ENDPOINT_URL", "https://legacy.example.test")
	if got := defaultS3EndpointURL(); got != "https://generic.example.test" {
		t.Fatalf("defaultS3EndpointURL() = %q, want generic endpoint", got)
	}

	t.Setenv("AWS_ENDPOINT_URL_S3", "https://ceph-rgw.example.test")
	if got := defaultS3EndpointURL(); got != "https://ceph-rgw.example.test" {
		t.Fatalf("defaultS3EndpointURL() = %q, want S3-specific endpoint", got)
	}
}

func TestParseTLSConditionFlags(t *testing.T) {
	cond, err := parseTLSCondition([]string{"tls", "api.example.com:443", "--servername", "api.internal", "--valid-for", "30d"})
	if err != nil {
		t.Fatalf("parseTLSCondition() error = %v", err)
	}
	tlsCond, ok := cond.(*condition.TLSCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.TLSCondition", cond)
	}
	if tlsCond.Address != "api.example.com:443" || tlsCond.ServerName != "api.internal" {
		t.Fatalf("tls condition = %+v", tlsCond)
	}
	if tlsCond.ValidFor != 30*24*time.Hour {
		t.Fatalf("ValidFor = %s, want 720h", tlsCond.ValidFor)
	}
}

func TestParseTLSConditionErrors(t *testing.T) {
	badCA := filepath.Join(t.TempDir(), "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing address", segment: []string{"tls"}, wantErr: "exactly one"},
		{name: "bad address", segment: []string{"tls", "api.example.com"}, wantErr: "invalid tls address"},
		{name: "bad valid for", segment: []string{"tls", "api.example.com:443", "--valid-for", "soon"}, wantErr: "invalid --valid-for"},
		{name: "negative valid for", segment: []string{"tls", "api.example.com:443", "--valid-for", "-1s"}, wantErr: "invalid --valid-for"},
		{name: "bad ca", segment: []string{"tls", "api.example.com:443", "--ca-file", badCA}, wantErr: "no PEM certificates"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTLSCondition(tt.segment)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
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
		{name: "deleted contains", args: []string{"file", "/tmp/f", "--deleted", "--contains", "gone"}, wantErr: "--deleted cannot be combined"},
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

func TestParseProcessConditionFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantPID   int
		wantName  string
		wantState condition.ProcessState
		wantErr   string
	}{
		{name: "pid running default", args: []string{"process", "--pid", "42"}, wantPID: 42, wantState: condition.ProcessRunning},
		{name: "name running explicit", args: []string{"process", "--name", "postgres", "--running"}, wantName: "postgres", wantState: condition.ProcessRunning},
		{name: "stopped", args: []string{"process", "--pid", "42", "--stopped"}, wantPID: 42, wantState: condition.ProcessStopped},
		{name: "missing selector", args: []string{"process", "--running"}, wantErr: "exactly one"},
		{name: "both selectors", args: []string{"process", "--pid", "42", "--name", "postgres"}, wantErr: "mutually exclusive"},
		{name: "bad pid", args: []string{"process", "--pid", "-1"}, wantErr: "positive"},
		{name: "state conflict", args: []string{"process", "--pid", "42", "--running", "--stopped"}, wantErr: "mutually exclusive"},
		{name: "positional", args: []string{"process", "postgres", "--running"}, wantErr: "positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := parseProcessCondition(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcessCondition() error = %v", err)
			}
			pc, ok := cond.(*condition.ProcessCondition)
			if !ok {
				t.Fatalf("condition type = %T, want *condition.ProcessCondition", cond)
			}
			if pc.PID != tt.wantPID || pc.Name != tt.wantName || pc.State != tt.wantState {
				t.Fatalf("process condition = %+v, want pid=%d name=%q state=%q", pc, tt.wantPID, tt.wantName, tt.wantState)
			}
		})
	}
}

func TestParseSystemdConditionFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantUnit  string
		wantState condition.SystemdState
		wantErr   string
	}{
		{name: "active default", args: []string{"systemd", "nginx.service"}, wantUnit: "nginx.service", wantState: condition.SystemdActive},
		{name: "active explicit", args: []string{"systemd", "nginx.service", "--active"}, wantUnit: "nginx.service", wantState: condition.SystemdActive},
		{name: "inactive", args: []string{"systemd", "nginx.service", "--inactive"}, wantUnit: "nginx.service", wantState: condition.SystemdInactive},
		{name: "failed", args: []string{"systemd", "nginx.service", "--failed"}, wantUnit: "nginx.service", wantState: condition.SystemdFailed},
		{name: "missing unit", args: []string{"systemd", "--active"}, wantErr: "exactly one UNIT"},
		{name: "extra positional", args: []string{"systemd", "nginx.service", "extra"}, wantErr: "exactly one UNIT"},
		{name: "state conflict", args: []string{"systemd", "nginx.service", "--active", "--failed"}, wantErr: "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond, err := parseSystemdCondition(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSystemdCondition() error = %v", err)
			}
			sc, ok := cond.(*condition.SystemdCondition)
			if !ok {
				t.Fatalf("condition type = %T, want *condition.SystemdCondition", cond)
			}
			if sc.Unit != tt.wantUnit || sc.State != tt.wantState {
				t.Fatalf("systemd condition = %+v, want unit=%q state=%q", sc, tt.wantUnit, tt.wantState)
			}
		})
	}
}

func TestParseExecConditionRejectsInvalidJSONPath(t *testing.T) {
	_, err := parseExecCondition([]string{"exec", "--jsonpath", "ready == true", "--", "/bin/sh", "-c", "printf '{}'"})
	if err == nil {
		t.Fatal("parseExecCondition() expected invalid jsonpath error, got nil")
	}
	if !strings.Contains(err.Error(), "jsonpath must start") {
		t.Fatalf("err = %q, want jsonpath error", err)
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
		{"nan jitter", []string{"--jitter", "NaN", "file", "x"}, "jitter"},
		{"infinite jitter", []string{"--jitter", "+Inf", "file", "x"}, "jitter"},
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
	opts, err := parseDoctorOptions([]string{"--output", "json", "--require", "docker,k8s", "--require", "dns-wire", "--backend", "docker,k8s", "--backend", "dns"})
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
	if strings.Join(opts.backends, ",") != "docker,k8s,dns" {
		t.Fatalf("backends = %v", opts.backends)
	}
}

func TestParseDoctorOptionsErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "bad output", args: []string{"--output", "xml"}},
		{name: "bad require", args: []string{"--require", "printer"}},
		{name: "bad backend", args: []string{"--backend", "not-a-backend"}},
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

func TestDoctorBackendChecks(t *testing.T) {
	for _, backend := range []string{"docker", "k8s", "dns", "systemd", "launchd", "cosign", "icmp", "exec", "http"} {
		t.Run(backend, func(t *testing.T) {
			check := doctorBackendCheck(t.Context(), backend)
			if check.Name != "backend:"+backend {
				t.Fatalf("check name = %q", check.Name)
			}
			if check.Status == "" {
				t.Fatalf("check status empty: %+v", check)
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
	if got := combineDoctorStatus(doctorWarn, doctorCheck{Status: doctorOK}); got != doctorWarn {
		t.Fatalf("existing warn status = %s, want warn", got)
	}
}

func TestRunDoctorWithRequiredInjectedFailure(t *testing.T) {
	deps := doctorDeps{checks: []doctorCheckFunc{
		func(context.Context) doctorCheck {
			return doctorCheck{Name: "temp", Status: doctorOK, Detail: "ok"}
		},
		func(context.Context) doctorCheck {
			return doctorCheck{Name: "docker", Status: doctorWarn, Detail: "offline"}
		},
	}}
	var stdout, stderr bytes.Buffer
	code, err := runDoctorWithDeps(t.Context(), []string{"--output", "json", "--require", "docker"}, &stdout, &stderr, deps)
	if err != nil {
		t.Fatalf("runDoctorWithDeps() error = %v", err)
	}
	if code != ExitFatal {
		t.Fatalf("exit code = %d, want %d", code, ExitFatal)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid json: %v: %s", err, stdout.String())
	}
	if report.Status != doctorFail {
		t.Fatalf("status = %s, want fail", report.Status)
	}
	if len(report.Checks) != 2 || !report.Checks[1].Required {
		t.Fatalf("checks = %+v, want injected required docker check", report.Checks)
	}
}

func TestRunDoctorAdditionalBranches(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runDoctorWithDeps(t.Context(), []string{"help"}, &stdout, &stderr, doctorDeps{})
	if err != nil || code != ExitSatisfied || !strings.Contains(stdout.String(), "waitfor doctor") {
		t.Fatalf("doctor help code=%d err=%v stdout=%q", code, err, stdout.String())
	}
	stdout.Reset()
	code, err = runDoctorWithDeps(t.Context(), []string{"--bad"}, &stdout, &stderr, doctorDeps{})
	if err == nil || code != ExitInvalid {
		t.Fatalf("doctor bad option code=%d err=%v, want invalid error", code, err)
	}
	code, err = runDoctorWithDeps(t.Context(), []string{"--output", "json"}, errWriter{}, &stderr, doctorDeps{})
	if err == nil || code != ExitFatal {
		t.Fatalf("doctor write error code=%d err=%v, want fatal error", code, err)
	}
}

func TestBuildDoctorReportUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	deps := doctorDeps{checks: []doctorCheckFunc{
		func(ctx context.Context) doctorCheck {
			if ctx.Err() == nil {
				t.Fatal("doctor check received uncancelled context")
			}
			return doctorCheck{Name: "temp", Status: doctorOK}
		},
	}}

	report := buildDoctorReport(ctx, map[string]bool{"temp": true}, nil, deps)
	if report.Status != doctorOK {
		t.Fatalf("status = %s, want ok", report.Status)
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
	_, err := runDoctorCommand(t.Context(), "definitely-no-such-waitfor-doctor-command")
	if err == nil {
		t.Fatal("runDoctorCommand() expected error, got nil")
	}
}

func TestDoctorLimitedBufferTruncates(t *testing.T) {
	var buf doctorLimitedBuffer
	_, _ = buf.Write([]byte(strings.Repeat("x", maxDoctorCommandOutput+1)))
	_, _ = buf.Write([]byte("ignored"))
	got := buf.String()
	if !strings.Contains(got, "truncated") {
		t.Fatalf("buffer suffix = %q, want truncation marker", got[len(got)-32:])
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

func TestParseConditionNameDoesNotConsumeProcessName(t *testing.T) {
	cond, err := parseCondition([]string{"process", "--name", "postgres", "--running"})
	if err != nil {
		t.Fatalf("parseCondition() error = %v", err)
	}
	processCond, ok := cond.(*condition.ProcessCondition)
	if !ok {
		t.Fatalf("condition type = %T, want process condition", cond)
	}
	if processCond.Name != "postgres" {
		t.Fatalf("process name = %q, want postgres", processCond.Name)
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

func TestParseLogConditionRejectsInvalidJSONPath(t *testing.T) {
	_, err := parseLogCondition([]string{"log", filepath.Join(t.TempDir(), "app.log"), "--jsonpath", "ready == true"})
	if err == nil {
		t.Fatal("parseLogCondition() expected invalid jsonpath error, got nil")
	}
	if !strings.Contains(err.Error(), "jsonpath must start") {
		t.Fatalf("err = %q, want jsonpath error", err)
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
		{"bad jsonpath path", []string{"k8s", "pod/a", "--jsonpath", "ready == true"}, "jsonpath must start"},
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

// ── parseUnixCondition ───────────────────────────────────────────────────────

func TestParseUnixConditionSuccess(t *testing.T) {
	cond, err := parseUnixCondition([]string{"unix", "/var/run/docker.sock"})
	if err != nil {
		t.Fatalf("parseUnixCondition() error = %v", err)
	}
	unixCond, ok := cond.(*condition.UnixCondition)
	if !ok {
		t.Fatalf("condition type = %T, want *condition.UnixCondition", cond)
	}
	if unixCond.Path != "/var/run/docker.sock" {
		t.Fatalf("path = %q, want /var/run/docker.sock", unixCond.Path)
	}
}

func TestParseUnixConditionErrors(t *testing.T) {
	tests := []struct {
		name    string
		segment []string
		wantErr string
	}{
		{name: "missing path", segment: []string{"unix"}, wantErr: "exactly one"},
		{name: "blank path", segment: []string{"unix", "  "}, wantErr: "path is required"},
		{name: "too many args", segment: []string{"unix", "/tmp/a.sock", "/tmp/b.sock"}, wantErr: "exactly one"},
		{name: "unknown flag", segment: []string{"unix", "/tmp/a.sock", "--bad"}, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseUnixCondition(tt.segment)
			if err == nil {
				t.Fatal("parseUnixCondition() expected error, got nil")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseUnixCondition() err = %q, want to contain %q", err, tt.wantErr)
			}
		})
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
		{"bad host whitespace", []string{"dns", "bad name"}, "invalid dns name"},
		{"bad host control", []string{"dns", "bad\tname"}, "invalid dns name"},
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
		{"empty bracket server", []string{"dns", "example.com", "--server", "[]"}, "invalid dns server address"},
		{"space server", []string{"dns", "example.com", "--server", "bad host"}, "invalid dns server address"},
		{"bad server port", []string{"dns", "example.com", "--server", "host:abc"}, "port must be between 1 and 65535"},
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

func TestNormalizeDNSServerFromCLI(t *testing.T) {
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
			got, err := condition.NormalizeDNSServer(tt.input)
			if err != nil {
				t.Fatalf("NormalizeDNSServer(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeDNSServer(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeDNSServerErrorsFromCLI(t *testing.T) {
	tests := []string{
		"host:",
		"host:abc",
		"host:0",
		"host:70000",
		"[::1]:abc",
		"[]",
		"bad host",
		"::1 bad",
		" host",
		" ",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if _, err := condition.NormalizeDNSServer(input); err == nil {
				t.Fatalf("NormalizeDNSServer(%q) expected error, got nil", input)
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

func TestParseHTTPHeadersRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{name: "bad name", input: []string{"Bad Header: value"}, want: "invalid HTTP header"},
		{name: "newline", input: []string{"X-Test: ok\nbad"}, want: "invalid HTTP header"},
		{name: "control", input: []string{"X-Test: bad\x01"}, want: "invalid HTTP header"},
		{name: "duplicate", input: []string{"X-Test=one", "x-test=two"}, want: "duplicate HTTP header"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseHTTPHeaders(tt.input)
			if err == nil {
				t.Fatal("parseHTTPHeaders() expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %q, want %q", err, tt.want)
			}
		})
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

func TestReadFileLimitBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readFileLimit(path, 6); err != nil || string(data) != "abcdef" {
		t.Fatalf("readFileLimit exact = %q err=%v", data, err)
	}
	if _, err := readFileLimit(path, 5); err == nil {
		t.Fatal("oversized readFileLimit succeeded")
	}
	if _, err := readFileLimit(filepath.Join(t.TempDir(), "missing.txt"), 10); err == nil {
		t.Fatal("missing readFileLimit succeeded")
	}
	if _, err := readFileLimit(t.TempDir(), 10); err == nil {
		t.Fatal("directory readFileLimit succeeded")
	}
}

func TestAdditionalCLIHelperBranches(t *testing.T) {
	if _, err := applyProfileDefaults(globalOptions{profile: "prod"}); err == nil {
		t.Fatal("unknown profile succeeded")
	}
	if got, err := parseJitter("25%"); err != nil || got != 0.25 {
		t.Fatalf("parseJitter percent = %v err=%v", got, err)
	}
	if _, err := parseJitter("bad%"); err == nil {
		t.Fatal("bad percent jitter succeeded")
	}
	headers := map[string]string{"authorization": "Bearer x"}
	if _, err := validateHTTPAuthSelection(headers, "new-token", "", ""); err == nil {
		t.Fatal("authorization helper with existing header succeeded")
	}
	if _, err := validateHTTPAuthSelection(nil, "", "user", ""); err == nil {
		t.Fatal("incomplete basic auth succeeded")
	}
	if cert, err := loadHTTPClientCertificate("", ""); err != nil || cert != nil {
		t.Fatalf("empty client certificate = %+v err=%v, want nil", cert, err)
	}
	if _, err := parseTLSValidFor("-1s"); err == nil {
		t.Fatal("negative TLS valid-for succeeded")
	}
	if _, err := parseTLSRootCAs(filepath.Join(t.TempDir(), "missing-ca.pem")); err == nil {
		t.Fatal("missing TLS CA file succeeded")
	}
	if err := validateS3EndpointURL("https://example.test/path?x=1"); err == nil {
		t.Fatal("S3 endpoint query succeeded")
	}
	t.Setenv("AWS_REGION", "us-west-2")
	if got := defaultS3Region(); got != "us-west-2" {
		t.Fatalf("defaultS3Region AWS_REGION = %q", got)
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "eu-central-1")
	if got := defaultS3Region(); got != "eu-central-1" {
		t.Fatalf("defaultS3Region AWS_DEFAULT_REGION = %q", got)
	}
	for _, size := range []int{-1, 65536} {
		if _, err := checkedDNSUDPSize(size); err == nil {
			t.Fatalf("checkedDNSUDPSize(%d) succeeded", size)
		}
	}
	for raw, want := range map[string]string{
		"po":          "pod",
		"deployments": "deployment",
		"sts":         "statefulset",
		"ds":          "daemonset",
		"jobs":        "job",
		"Custom":      "custom",
	} {
		if got := normalizeKubernetesKind(raw); got != want {
			t.Fatalf("normalizeKubernetesKind(%q) = %q, want %q", raw, got, want)
		}
	}
	if kubernetesWaitSupportsKind("complete", "pod") {
		t.Fatal("pod supported for job completion wait")
	}
	if isValueForPreviousFlag([]string{"--method=GET", "--"}, 1) {
		t.Fatal("inline flag assignment treated separator as previous flag value")
	}
	if got := dockerReason("container health unhealthy"); got != "unhealthy" {
		t.Fatalf("dockerReason health = %q", got)
	}
	if got := kubernetesReason("forbidden"); got != "auth" {
		t.Fatalf("kubernetesReason forbidden = %q", got)
	}
	backends, err := parseDoctorBackends([]string{"http,dns", " file "})
	if err != nil || !containsString(backends, "http") || !containsString(backends, "file") {
		t.Fatalf("parseDoctorBackends = %v err=%v", backends, err)
	}
	if _, err := parseDoctorBackends([]string{"unknown"}); err == nil {
		t.Fatal("unknown doctor backend succeeded")
	}
	required := map[string]bool{}
	if err := requireDoctorChecks(required, "temp,,dns-wire"); err != nil || !required["temp"] || !required["dns-wire"] {
		t.Fatalf("requireDoctorChecks required=%v err=%v", required, err)
	}
	if err := requireDoctorChecks(required, "bogus"); err == nil {
		t.Fatal("bad required doctor check succeeded")
	}
	t.Setenv("SHELL", "/no/such/shell")
	if check := shellCheck(t.Context()); check.Status != doctorWarn {
		t.Fatalf("shellCheck status = %s, want warn", check.Status)
	}
	if err := runBackendHelp([]string{}, io.Discard); err == nil {
		t.Fatal("backend help without backend succeeded")
	}
	if err := runBackendHelp([]string{"unknown"}, io.Discard); err == nil {
		t.Fatal("unknown backend help succeeded")
	}
	if err := runCompletion([]string{"powershell"}, io.Discard); err == nil {
		t.Fatal("unsupported completion shell succeeded")
	}
	if value, ok := conditionFlagValue([]string{"--name"}, 0, 1); ok || value != "" {
		t.Fatalf("conditionFlagValue missing value = %q/%v, want empty false", value, ok)
	}
	if backends, err := parseDoctorBackends([]string{","}); err != nil || len(backends) != 0 {
		t.Fatalf("empty doctor backends = %v err=%v, want none", backends, err)
	}
	if code, err := writeExplain(errWriter{}, io.Discard, globalOptions{format: output.FormatJSON}, runner.Config{}); code != ExitSatisfied || err == nil {
		t.Fatalf("writeExplain json error code=%d err=%v, want satisfied code with write error", code, err)
	}
	if code, err := writeExplain(errWriter{}, io.Discard, globalOptions{format: output.FormatNDJSON}, runner.Config{}); code != ExitFatal || err == nil {
		t.Fatalf("writeExplain ndjson error code=%d err=%v, want fatal error", code, err)
	}
	if role := conditionRoleForExplain(condition.NewFile("/tmp/ready", condition.FileExists)); role != condition.RoleReady {
		t.Fatalf("conditionRoleForExplain ready = %s, want ready", role)
	}
	if role := conditionRoleForExplain(explainWrapper{inner: condition.NewGuard(condition.NewFile("/tmp/wrapped", condition.FileExists))}); role != condition.RoleGuard {
		t.Fatalf("conditionRoleForExplain wrapper = %s, want guard", role)
	}
	wrappedGuard := condition.WithName(condition.NewGuard(condition.NewFile("/tmp/blocked", condition.FileExists)), "blocked")
	if role := conditionRoleForExplain(wrappedGuard); role != condition.RoleGuard {
		t.Fatalf("conditionRoleForExplain = %s, want guard", role)
	}
	var explainOut bytes.Buffer
	writeExplainText(&explainOut, explainReport{
		Mode:              "any",
		Backoff:           "exponential",
		Jitter:            0.1,
		Profile:           "ci",
		ConfigFile:        "waitfor.yaml",
		TimeoutSeconds:    1,
		IntervalSeconds:   0.1,
		Conditions:        []explainConditionReport{{Backend: "file", Target: "/tmp/ready", Name: "ready"}, {Backend: "log", Target: "/tmp/app.log", Name: "guard log", Guard: true}},
		RequiredSuccesses: 1,
	})
	if !strings.Contains(explainOut.String(), "jitter=0.100") || !strings.Contains(explainOut.String(), "[guard]") {
		t.Fatalf("explain text = %q", explainOut.String())
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("write failed")
}

type explainWrapper struct {
	inner condition.Condition
}

func (w explainWrapper) Descriptor() condition.Descriptor {
	return w.inner.Descriptor()
}

func (w explainWrapper) Check(ctx context.Context) condition.Result {
	return w.inner.Check(ctx)
}

func (w explainWrapper) UnwrapCondition() condition.Condition {
	return w.inner
}
