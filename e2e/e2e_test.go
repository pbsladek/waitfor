package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/cli"
	"github.com/pbsladek/wait-for/internal/output"
)

// execute runs the CLI with the given args and returns exit code, stdout, stderr.
func execute(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if args == nil {
		args = []string{}
	}
	code := cli.Execute(ctx, args, nil, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// mustCode asserts the exit code and returns stdout, stderr for further inspection.
func mustCode(t *testing.T, want int, args ...string) (string, string) {
	t.Helper()
	code, stdout, stderr := execute(t, args...)
	if code != want {
		t.Fatalf("exit code = %d, want %d\nstdout: %q\nstderr: %q", code, want, stdout, stderr)
	}
	return stdout, stderr
}

// newTCPListener starts a TCP listener on a random port and accepts connections
// in the background. Closes automatically when the test ends.
func newTCPListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return l.Addr().String()
}

// refusedAddr returns a host:port that is guaranteed to refuse connections.
func refusedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// ── HTTP ────────────────────────────────────────────────────────────────────

func TestHTTPSatisfied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL)
}

func TestHTTPStatusClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--status", "2xx")
}

func TestHTTPStatusMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "http", server.URL)
}

func TestHTTPMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--method", "POST")
}

func TestHTTPHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--header", "X-Token=secret")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"http", server.URL, "--header", "X-Token=wrong")
}

func TestHTTPHeaderColonSeparator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--header", "Accept: application/json")
}

func TestHTTPRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		if buf.String() != "ping" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--method", "POST", "--body", "ping")
}

func TestHTTPRequestBodyFile(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(body, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		if buf.String() != "from-file" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--method", "POST", "--body-file", body)
}

func TestHTTPBodyContains(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"status":"ready"}`)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--body-contains", "ready")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"http", server.URL, "--body-contains", "not-there")
}

func TestHTTPBodyRegex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "service is ready")
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--body-matches", `ready$`)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"http", server.URL, "--body-matches", `^notthis`)
}

func TestHTTPJSONPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ready":true,"replicas":3}`)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--jsonpath", ".ready == true")
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--jsonpath", ".replicas >= 3")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"http", server.URL, "--jsonpath", ".ready == false")
}

func TestHTTPNoRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ready", http.StatusFound)
	}))
	defer server.Close()
	// Without flag: follows redirect, gets 200 (redirect target may 404 but that's still a response)
	// With flag + 3xx matcher: captures the redirect status directly
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--no-follow-redirects", "--status", "3xx")
}

func TestHTTPInsecure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	// Without --insecure: TLS verification fails → unsatisfied → timeout
	mustCode(t, cli.ExitTimeout, "--timeout", "200ms", "--interval", "50ms", "http", server.URL)
	// With --insecure: succeeds
	mustCode(t, cli.ExitSatisfied, "http", server.URL, "--insecure")
}

func TestHTTPUnreachable(t *testing.T) {
	mustCode(t, cli.ExitTimeout, "--timeout", "100ms", "--interval", "20ms",
		"http", "http://127.0.0.1:1")
}

func TestHTTPStderrOnTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, "not ready yet")
	}))
	defer server.Close()
	_, stderr := mustCode(t, cli.ExitTimeout,
		"--timeout", "100ms", "--interval", "20ms",
		"http", server.URL)
	if !strings.Contains(stderr, "503") {
		t.Fatalf("stderr %q does not mention status 503", stderr)
	}
}

// ── TCP ─────────────────────────────────────────────────────────────────────

func TestTCPSatisfied(t *testing.T) {
	addr := newTCPListener(t)
	mustCode(t, cli.ExitSatisfied, "tcp", addr)
}

func TestTCPRefused(t *testing.T) {
	addr := refusedAddr(t)
	mustCode(t, cli.ExitTimeout, "--timeout", "100ms", "--interval", "20ms", "tcp", addr)
}

func TestTCPInvalidAddress(t *testing.T) {
	_, stderr := mustCode(t, cli.ExitInvalid, "tcp", "not-an-address")
	if stderr == "" {
		t.Fatal("expected error on stderr for invalid address")
	}
}

// ── File ────────────────────────────────────────────────────────────────────

func TestFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "file", path, "--exists")
}

func TestFileExistsTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "file", path, "--exists")
}

func TestFileDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "will-be-deleted")
	mustCode(t, cli.ExitSatisfied, "file", path, "--deleted")
}

func TestFileDeletedTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent")
	if err := os.WriteFile(path, []byte("still here"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "file", path, "--deleted")
}

func TestFileNonEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "content")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "file", path, "--nonempty")
}

func TestFileNonEmptyEmptyFileTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "file", path, "--nonempty")
}

func TestFileContains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("service: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "file", path, "--contains", "ready")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"file", path, "--contains", "not-in-file")
}

func TestFileMutuallyExclusiveStateFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	_, stderr := mustCode(t, cli.ExitInvalid, "file", path, "--exists", "--deleted")
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr %q does not mention mutually exclusive", stderr)
	}
}

// ── Log ─────────────────────────────────────────────────────────────────────

func TestLogContainsSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path, "--contains", "ready", "--from-start")
}

func TestLogContainsTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: starting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"log", path, "--contains", "ready", "--from-start")
}

func TestLogMatchesSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("2024-01-01 ERROR: connection timeout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path, "--matches", `ERROR:.*timeout`, "--from-start")
}

func TestLogJSONExprSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	lines := `{"level":"info","msg":"starting"}` + "\n" + `{"level":"ready","msg":"up"}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path, "--jsonpath", `.level == "ready"`, "--from-start")
}

func TestLogNewContentOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// Write "ready" before waitfor starts — should be skipped without --from-start.
	if err := os.WriteFile(path, []byte("service: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"log", path, "--contains", "ready")
}

func TestLogTailsNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Append matching content after a short delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- test appends to path created by t.TempDir.
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString("service: ready\n")
	}()

	mustCode(t, cli.ExitSatisfied, "--timeout", "2s", "--interval", "10ms",
		"log", path, "--contains", "ready")
}

func TestLogMissingMatcherIsInvalid(t *testing.T) {
	mustCode(t, cli.ExitInvalid, "log", "/tmp/app.log")
}

func TestLogInvalidRegex(t *testing.T) {
	mustCode(t, cli.ExitInvalid, "log", "/tmp/app.log", "--matches", "[invalid")
}

func TestLogTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// "ready" is on line 3 of 4; --tail 2 covers lines 3 and 4.
	content := "line one\nline two\nline three: ready\nline four\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path, "--contains", "ready", "--tail", "2")
}

func TestLogTailExcludesOlderLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// "ready" is only in line one; --tail 2 should not reach it.
	content := "line one: ready\nline two\nline three\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"log", path, "--contains", "ready", "--tail", "2")
}

func TestLogFromStartAndTailMutuallyExclusive(t *testing.T) {
	mustCode(t, cli.ExitInvalid, "log", "/tmp/app.log", "--contains", "x",
		"--from-start", "--tail", "5")
}

func TestLogMinMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// Write 3 matching lines up front; --min-matches 3 should be satisfied in one poll.
	if err := os.WriteFile(path, []byte("heartbeat\nheartbeat\nheartbeat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path, "--contains", "heartbeat",
		"--min-matches", "3", "--from-start")
}

func TestLogMinMatchesAcrossPolls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("heartbeat\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Append two more matches after a short delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- test appends to path created by t.TempDir.
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString("heartbeat\nheartbeat\n")
	}()

	mustCode(t, cli.ExitSatisfied, "--timeout", "2s", "--interval", "10ms",
		"log", path, "--contains", "heartbeat", "--min-matches", "3", "--from-start")
}

func TestLogMinMatchesTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("heartbeat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"log", path, "--contains", "heartbeat", "--min-matches", "3", "--from-start")
}

func TestLogExclude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// "ready" appears on DEBUG lines (excluded) and one INFO line (matches).
	content := "DEBUG ready check\nINFO service ready\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "log", path,
		"--contains", "ready", "--exclude", `^DEBUG`, "--from-start")
}

func TestLogExcludeBlocksAllLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("DEBUG ready\nDEBUG ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"log", path, "--contains", "ready", "--exclude", `^DEBUG`, "--from-start")
}

func TestLogInvalidExcludeRegex(t *testing.T) {
	mustCode(t, cli.ExitInvalid, "log", "/tmp/app.log", "--contains", "x", "--exclude", "[bad")
}

func TestLogMatchedLineInJSONDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service ready at port 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ := mustCode(t, cli.ExitSatisfied, "--output", "json",
		"log", path, "--contains", "ready", "--from-start")
	if !strings.Contains(stdout, "port 8080") {
		t.Fatalf("JSON output %q does not contain matched line content", stdout)
	}
}

func TestGuardLogFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("INFO boot\nFATAL crash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitFatal,
		"--timeout", "500ms",
		"--interval", "10ms",
		"file", filepath.Join(t.TempDir(), "missing"), "--exists",
		"--", "guard", "log", path, "--contains", "FATAL", "--from-start")
}

// ── Exec ────────────────────────────────────────────────────────────────────

func TestExecSuccess(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec", "--", "/bin/sh", "-c", "exit 0")
}

func TestExecExpectedExitCode(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec", "--exit-code", "7", "--", "/bin/sh", "-c", "exit 7")
}

func TestExecExitCodeMismatch(t *testing.T) {
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--", "/bin/sh", "-c", "exit 1")
}

func TestExecOutputContains(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec", "--output-contains", "hello", "--", "/bin/sh", "-c", "echo hello")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--output-contains", "missing", "--", "/bin/sh", "-c", "echo hello")
}

func TestExecJSONPath(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec", "--jsonpath", ".ready == true",
		"--", "/bin/sh", "-c", `printf '{"ready":true}'`)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--jsonpath", ".ready == true",
		"--", "/bin/sh", "-c", `printf '{"ready":false}'`)
}

func TestExecCwd(t *testing.T) {
	dir := t.TempDir()
	mustCode(t, cli.ExitSatisfied, "exec", "--cwd", dir, "--output-contains", dir,
		"--", "/bin/sh", "-c", "pwd")
}

func TestExecEnv(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec",
		"--env", "WAITFOR_E2E=yep",
		"--output-contains", "yep",
		"--", "/bin/sh", "-c", "echo $WAITFOR_E2E")
}

func TestExecMaxOutputBytes(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "exec",
		"--max-output-bytes", "3",
		"--output-contains", "abc",
		"--", "/bin/sh", "-c", "printf abcdefgh")
}

func TestExecCommandNotFound(t *testing.T) {
	mustCode(t, cli.ExitFatal, "exec", "--", "/definitely/no/such/command")
}

func TestExecTimeout(t *testing.T) {
	mustCode(t, cli.ExitTimeout, "--timeout", "100ms", "--interval", "10ms", "--attempt-timeout", "20ms",
		"exec", "--", "/bin/sh", "-c", "sleep 5")
}

// ── Global flags ─────────────────────────────────────────────────────────────

func TestGlobalTimeout(t *testing.T) {
	start := time.Now()
	mustCode(t, cli.ExitTimeout, "--timeout", "100ms", "--interval", "50ms",
		"file", filepath.Join(t.TempDir(), "missing"), "--exists")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("took %s, expected close to 100ms", elapsed)
	}
}

func TestGlobalInterval(t *testing.T) {
	// Count how many attempts happen in a short timeout with a large interval
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	mustCode(t, cli.ExitTimeout, "--timeout", "120ms", "--interval", "50ms", "http", server.URL)
	// With 50ms interval and 120ms timeout, we expect 2-3 attempts, not 10+
	if attempts > 5 {
		t.Fatalf("too many attempts (%d) for 50ms interval / 120ms timeout", attempts)
	}
}

func TestGlobalAttemptTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()
	mustCode(t, cli.ExitTimeout,
		"--timeout", "250ms", "--interval", "10ms", "--attempt-timeout", "50ms",
		"http", slow.URL)
}

func TestGlobalSuccesses(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mustCode(t, cli.ExitSatisfied, "--successes", "3", "--interval", "1ms", "http", server.URL)
	if attempts < 3 {
		t.Fatalf("attempts = %d, want at least 3", attempts)
	}
}

func TestModeAll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	addr := newTCPListener(t)
	mustCode(t, cli.ExitSatisfied, "--mode", "all",
		"http", server.URL,
		"--", "tcp", addr)
}

func TestModeAnyFirstSatisfies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	missing := filepath.Join(t.TempDir(), "missing")
	mustCode(t, cli.ExitSatisfied, "--mode", "any", "--timeout", "2s",
		"http", server.URL,
		"--", "file", missing, "--exists")
}

func TestModeAllTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	missing := filepath.Join(t.TempDir(), "missing")
	mustCode(t, cli.ExitTimeout, "--mode", "all", "--timeout", "100ms", "--interval", "20ms",
		"http", server.URL,
		"--", "file", missing, "--exists")
}

func TestVerboseShowsAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr := mustCode(t, cli.ExitSatisfied, "--verbose", "file", path, "--exists")
	if !strings.Contains(stderr, "[ok]") {
		t.Fatalf("verbose stderr %q does not contain [ok]", stderr)
	}
}

func TestJSONOutputToStdout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := mustCode(t, cli.ExitSatisfied, "--output", "json", "file", path, "--exists")
	if stderr != "" {
		t.Fatalf("stderr should be empty in JSON mode, got %q", stderr)
	}
	var report output.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if report.Status != "satisfied" {
		t.Fatalf("status = %q, want satisfied", report.Status)
	}
	if !report.Satisfied {
		t.Fatal("satisfied = false, want true")
	}
	if report.ElapsedSeconds < 0 {
		t.Fatalf("elapsed_seconds = %v, want >= 0", report.ElapsedSeconds)
	}
	if len(report.Conditions) != 1 {
		t.Fatalf("len(conditions) = %d, want 1", len(report.Conditions))
	}
	c := report.Conditions[0]
	if c.Backend != "file" {
		t.Fatalf("backend = %q, want file", c.Backend)
	}
	if !c.Satisfied {
		t.Fatal("condition satisfied = false")
	}
	if c.Attempts < 1 {
		t.Fatal("attempts < 1")
	}
}

func TestJSONOutputOnTimeout(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	stdout, _ := mustCode(t, cli.ExitTimeout,
		"--output", "json", "--timeout", "50ms", "--interval", "10ms",
		"file", missing, "--exists")
	var report output.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if report.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", report.Status)
	}
	if report.Satisfied {
		t.Fatal("satisfied = true, want false")
	}
	if report.Conditions[0].LastError == "" {
		t.Fatal("last_error is empty, want an error message")
	}
}

func TestJSONOutputOnFatal(t *testing.T) {
	stdout, _ := mustCode(t, cli.ExitFatal,
		"--output", "json",
		"exec", "--", "/no/such/binary")
	var report output.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if report.Status != "fatal" {
		t.Fatalf("status = %q, want fatal", report.Status)
	}
	if !report.Conditions[0].Fatal {
		t.Fatal("condition fatal = false, want true")
	}
}

func TestJSONPerAttemptTimeoutField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ := mustCode(t, cli.ExitSatisfied,
		"--output", "json", "--attempt-timeout", "5s",
		"file", path, "--exists")
	var report output.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if report.PerAttemptTimeoutSeconds <= 0 {
		t.Fatalf("per_attempt_timeout_seconds = %v, want > 0", report.PerAttemptTimeoutSeconds)
	}
}

// ── Multi-condition ──────────────────────────────────────────────────────────

func TestMultiConditionSeparator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	addr := newTCPListener(t)
	mustCode(t, cli.ExitSatisfied,
		"http", server.URL,
		"--", "tcp", addr)
}

func TestMultiConditionModeAny(t *testing.T) {
	// One satisfied, one never satisfies
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	mustCode(t, cli.ExitSatisfied, "--mode", "any",
		"http", server.URL,
		"--", "tcp", "127.0.0.1:1") // port 1 is refused
}

// ── Exit codes ───────────────────────────────────────────────────────────────

func TestExitCodeSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := execute(t, "file", path, "--exists")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestExitCodeTimeout(t *testing.T) {
	code, _, _ := execute(t, "--timeout", "50ms", "--interval", "10ms",
		"file", filepath.Join(t.TempDir(), "missing"), "--exists")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestExitCodeInvalid(t *testing.T) {
	code, _, _ := execute(t, "tcp", "not-a-port")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestExitCodeFatal(t *testing.T) {
	code, _, _ := execute(t, "exec", "--", "/no/such/binary")
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

// ── Error messages ────────────────────────────────────────────────────────────

func TestErrorGoesToStderr(t *testing.T) {
	code, stdout, stderr := execute(t, "tcp", "bad-address")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if stdout != "" {
		t.Fatalf("stdout should be empty, got %q", stdout)
	}
	if stderr == "" {
		t.Fatal("stderr should contain error message")
	}
}

func TestHelpText(t *testing.T) {
	code, stdout, _ := execute(t, "--help")
	if code != cli.ExitSatisfied {
		t.Fatalf("--help exit code = %d, want 0", code)
	}
	for _, want := range []string{"waitfor", "http", "tcp", "dns", "docker", "exec", "file", "k8s", "Exit codes"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help text missing %q", want)
		}
	}
}

func TestHelpExitCode130And143(t *testing.T) {
	_, stdout, _ := execute(t, "--help")
	if !strings.Contains(stdout, "130") {
		t.Fatal("help text missing exit code 130")
	}
	if !strings.Contains(stdout, "143") {
		t.Fatal("help text missing exit code 143")
	}
}

func TestNoArgsShowsHelp(t *testing.T) {
	code, stdout, _ := execute(t)
	if code != cli.ExitSatisfied {
		t.Fatalf("no-args exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "waitfor") {
		t.Fatal("no-args output missing help text")
	}
}

func TestUnknownBackendError(t *testing.T) {
	code, _, stderr := execute(t, "nope", "target")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "nope") {
		t.Fatalf("stderr %q does not mention unknown backend", stderr)
	}
}

func TestTimeoutOnStderr(t *testing.T) {
	_, stderr := mustCode(t, cli.ExitTimeout,
		"--timeout", "50ms", "--interval", "10ms",
		"file", filepath.Join(t.TempDir(), "missing"), "--exists")
	if !strings.Contains(stderr, "timeout") {
		t.Fatalf("stderr %q does not mention timeout", stderr)
	}
}

func TestInvalidFlagError(t *testing.T) {
	code, _, stderr := execute(t, "--timeout", "notaduration", "file", "x")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if stderr == "" {
		t.Fatal("expected error on stderr")
	}
}

func TestMutuallyExclusiveBodyFlags(t *testing.T) {
	code, _, stderr := execute(t, "http", "http://localhost", "--body", "x", "--body-file", "/tmp/f")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr %q does not mention mutual exclusion", stderr)
	}
}

func TestK8sMutuallyExclusiveFlags(t *testing.T) {
	code, _, stderr := execute(t, "k8s", "pod/myapp", "--condition", "Ready", "--jsonpath", ".status")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr %q does not mention mutually exclusive: %q", stderr, stderr)
	}
}

func TestInvalidHTTPURL(t *testing.T) {
	code, _, stderr := execute(t, "http", "not-a-url")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if stderr == "" {
		t.Fatal("expected error on stderr")
	}
}

func TestDNSLocalhost(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "dns", "localhost", "--type", "ANY", "--equals", "127.0.0.1", "--min-count", "1")
}

func TestDNSJSONOutput(t *testing.T) {
	stdout, stderr := mustCode(t, cli.ExitSatisfied, "--output", "json", "dns", "localhost", "--type", "ANY", "--min-count", "1")
	if stderr != "" {
		t.Fatalf("stderr should be empty in JSON mode, got %q", stderr)
	}
	var report output.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if report.Conditions[0].Backend != "dns" {
		t.Fatalf("backend = %q, want dns", report.Conditions[0].Backend)
	}
	if report.Conditions[0].Detail == "" {
		t.Fatal("dns detail is empty")
	}
}

func TestDNSInvalidType(t *testing.T) {
	code, _, stderr := execute(t, "dns", "localhost", "--type", "BOGUS")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid dns record type") {
		t.Fatalf("stderr %q does not mention invalid dns record type", stderr)
	}
}

func TestDNSInvalidRCode(t *testing.T) {
	code, _, stderr := execute(t, "dns", "localhost", "--resolver", "wire", "--server", "1.1.1.1", "--rcode", "READY")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid dns rcode") {
		t.Fatalf("stderr %q does not mention invalid dns rcode", stderr)
	}
}

func TestDNSAbsentMatcherConflict(t *testing.T) {
	code, _, stderr := execute(t, "dns", "localhost", "--absent", "--contains", "127.0.0.1")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "--absent cannot be combined") {
		t.Fatalf("stderr %q does not mention absent conflict", stderr)
	}
}

func TestDockerInvalidStatus(t *testing.T) {
	code, _, stderr := execute(t, "docker", "api", "--status", "warm")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid docker status") {
		t.Fatalf("stderr %q does not mention invalid docker status", stderr)
	}
}

func TestExecMissingSeparator(t *testing.T) {
	code, _, stderr := execute(t, "exec", "/bin/sh", "-c", "exit 0")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "--") {
		t.Fatalf("stderr %q does not mention -- separator", stderr)
	}
}
