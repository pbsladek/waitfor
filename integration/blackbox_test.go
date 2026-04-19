package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

var waitforBinary string

func TestMain(m *testing.M) {
	if os.Getenv("WAITFOR_BLACKBOX") != "1" {
		os.Exit(m.Run())
	}

	root, err := repoRoot()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "repo root: %v\n", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "waitfor-blackbox-*")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	waitforBinary = filepath.Join(dir, "waitfor")
	cmd := exec.Command("go", "build", "-o", waitforBinary, "./cmd/waitfor")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build waitfor: %v\n%s", err, stderr.String())
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file)), nil
}

func requireBlackbox(t *testing.T) {
	t.Helper()
	if os.Getenv("WAITFOR_BLACKBOX") != "1" {
		t.Skip("set WAITFOR_BLACKBOX=1 to run black-box binary integration tests")
	}
}

type commandResult struct {
	code   int
	stdout string
	stderr string
}

func runWaitfor(t *testing.T, args ...string) commandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, waitforBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("waitfor timed out in test harness: %v\nstdout: %q\nstderr: %q", ctx.Err(), stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("waitfor failed without exit status: %v\nstdout: %q\nstderr: %q", err, stdout.String(), stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return commandResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func requireExitCode(t *testing.T, got commandResult, want int) {
	t.Helper()
	if got.code != want {
		t.Fatalf("exit code = %d, want %d\nstdout: %q\nstderr: %q", got.code, want, got.stdout, got.stderr)
	}
}

func TestBinaryFilePolling(t *testing.T) {
	requireBlackbox(t)

	path := filepath.Join(t.TempDir(), "ready")
	timer := time.AfterFunc(150*time.Millisecond, func() {
		_ = os.WriteFile(path, []byte("ready\n"), 0o600)
	})
	defer timer.Stop()

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms", "file", path, "exists")
	requireExitCode(t, result, 0)
}

func TestBinaryFileStatesAndContains(t *testing.T) {
	requireBlackbox(t)

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready.log")
	if err := os.WriteFile(ready, []byte("service ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "file", ready, "exists"), 0)
	requireExitCode(t, runWaitfor(t, "file", ready, "nonempty"), 0)
	requireExitCode(t, runWaitfor(t, "file", ready, "--contains", "ready"), 0)
	requireExitCode(t, runWaitfor(t, "file", filepath.Join(dir, "missing"), "deleted"), 0)
}

func TestBinaryTimeoutExitCode(t *testing.T) {
	requireBlackbox(t)

	missing := filepath.Join(t.TempDir(), "missing")
	result := runWaitfor(t, "--timeout", "75ms", "--interval", "20ms", "file", missing, "exists")
	requireExitCode(t, result, 1)
}

func TestBinaryHTTPPolling(t *testing.T) {
	requireBlackbox(t)

	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms",
		"http", server.URL, "--jsonpath", ".ready == true")
	requireExitCode(t, result, 0)
	if attempts.Load() < 3 {
		t.Fatalf("attempts = %d, want polling before success", attempts.Load())
	}
}

func TestBinaryHTTPMethodHeadersBodyAndBodyFile(t *testing.T) {
	requireBlackbox(t)

	bodyPath := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(bodyPath, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readRequestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("X-Waitfor") != "yes" {
			http.Error(w, "bad method or header", http.StatusBadRequest)
			return
		}
		if body != "from-file" {
			http.Error(w, "bad body: "+body, http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, "service ready")
	}))
	defer server.Close()

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms",
		"http", server.URL,
		"--method", "POST",
		"--header", "X-Waitfor=yes",
		"--body-file", bodyPath,
		"--body-contains", "ready")
	requireExitCode(t, result, 0)
}

func readRequestBody(r *http.Request) (string, error) {
	var body bytes.Buffer
	if _, err := body.ReadFrom(r.Body); err != nil {
		return "", err
	}
	return body.String(), nil
}

func TestBinaryHTTPRedirectAndTLSFlags(t *testing.T) {
	requireBlackbox(t)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ready", http.StatusFound)
	}))
	defer redirect.Close()
	requireExitCode(t, runWaitfor(t, "http", redirect.URL, "--no-follow-redirects", "--status", "3xx"), 0)

	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tlsServer.Close()
	requireExitCode(t, runWaitfor(t, "http", tlsServer.URL, "--insecure", "--status", "204"), 0)
}

func TestBinaryTCPPolling(t *testing.T) {
	requireBlackbox(t)

	addr := freeTCPAddress(t)
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = listener.Close() }()
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		_ = conn.Close()
		errCh <- nil
	}()

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms", "tcp", addr)
	requireExitCode(t, result, 0)
	if err := <-errCh; err != nil {
		t.Fatalf("tcp listener: %v", err)
	}
}

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestBinaryModeAllAndAny(t *testing.T) {
	requireBlackbox(t)

	ready := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")

	requireExitCode(t, runWaitfor(t, "--mode", "any", "--timeout", "500ms", "--interval", "25ms",
		"file", missing, "exists",
		"--", "file", ready, "exists"), 0)
	requireExitCode(t, runWaitfor(t, "--mode", "all", "--timeout", "100ms", "--interval", "25ms",
		"file", ready, "exists",
		"--", "file", missing, "exists"), 1)
}

func TestBinaryExecPolling(t *testing.T) {
	requireBlackbox(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell script uses /bin/sh")
	}

	counter := filepath.Join(t.TempDir(), "count")
	script := `n=0; if [ -f "$1" ]; then n=$(cat "$1"); fi; n=$((n + 1)); printf "%s" "$n" > "$1"; if [ "$n" -ge 3 ]; then printf '{"ready":true}'; exit 0; fi; printf '{"ready":false}'; exit 1`
	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms",
		"exec", "--jsonpath", ".ready == true", "--", "/bin/sh", "-c", script, "waitfor-exec", counter)
	requireExitCode(t, result, 0)

	body, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if attempts < 3 {
		t.Fatalf("attempts = %d, want polling before success", attempts)
	}
}

func TestBinaryExecFatalExitCode(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "exec", "--", "/definitely/no/such/waitfor-command")
	requireExitCode(t, result, 3)
}

func TestBinaryJSONOutputStreams(t *testing.T) {
	requireBlackbox(t)

	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runWaitfor(t, "--output", "json", "file", path, "exists")
	requireExitCode(t, result, 0)
	if result.stderr != "" {
		t.Fatalf("stderr = %q, want empty for JSON output", result.stderr)
	}
	var payload struct {
		Status    string `json:"status"`
		Satisfied bool   `json:"satisfied"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, result.stdout)
	}
	if payload.Status != "satisfied" || !payload.Satisfied {
		t.Fatalf("payload = %+v, want satisfied", payload)
	}
}

func TestShellInvokedBinaryQuoting(t *testing.T) {
	requireBlackbox(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell invocation uses /bin/sh")
	}

	dir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ready file")
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `"$1" --output json file "$2" exists`, "sh", waitforBinary, path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("shell-invoked waitfor failed: %v\nstdout: %q\nstderr: %q", err, stdout.String(), stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty for JSON output", stderr.String())
	}
	var payload struct {
		Satisfied bool `json:"satisfied"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
	if !payload.Satisfied {
		t.Fatalf("payload = %+v, want satisfied", payload)
	}
}

func TestBinaryInvalidArgsExitCode(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "tcp", "not-a-host-port")
	requireExitCode(t, result, 2)
}

func TestBinaryKubernetesNamespacePolling(t *testing.T) {
	requireBlackbox(t)
	if os.Getenv("WAITFOR_BLACKBOX_K8S") != "1" {
		t.Skip("set WAITFOR_BLACKBOX_K8S=1 to run against a real Kubernetes cluster")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatalf("kubectl is required for Kubernetes black-box test: %v", err)
	}

	ns := fmt.Sprintf("waitfor-blackbox-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found=true")
	})

	createErr := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		createErr <- kubectl(ctx, "create", "namespace", ns)
	}()

	args := []string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "namespace/" + ns,
		"--jsonpath", ".metadata.name == " + ns,
	}
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	if err := <-createErr; err != nil {
		t.Fatalf("kubectl create namespace: %v", err)
	}
}

func TestBinaryKubernetesNamespaceTimeout(t *testing.T) {
	requireBlackbox(t)
	if os.Getenv("WAITFOR_BLACKBOX_K8S") != "1" {
		t.Skip("set WAITFOR_BLACKBOX_K8S=1 to run against a real Kubernetes cluster")
	}

	ns := fmt.Sprintf("waitfor-blackbox-missing-%d", time.Now().UnixNano())
	args := []string{
		"--timeout", "1s",
		"--interval", "200ms",
		"k8s", "namespace/" + ns,
		"--jsonpath", ".metadata.name == " + ns,
	}
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 1)
}

func kubectl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, output.String())
	}
	return nil
}
