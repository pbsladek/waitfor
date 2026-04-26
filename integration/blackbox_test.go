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
	cmd := exec.Command("go", "build", "-o", waitforBinary, "./cmd/waitfor") // #nosec G204 -- test harness builds the local fixed package path.
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

func requireKubernetesBlackbox(t *testing.T) {
	t.Helper()
	requireBlackbox(t)
	if os.Getenv("WAITFOR_BLACKBOX_K8S") != "1" {
		t.Skip("set WAITFOR_BLACKBOX_K8S=1 to run against a real Kubernetes cluster")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatalf("kubectl is required for Kubernetes black-box test: %v", err)
	}
}

func requireDockerBlackbox(t *testing.T) {
	t.Helper()
	requireBlackbox(t)
	if os.Getenv("WAITFOR_BLACKBOX_DOCKER") != "1" {
		t.Skip("set WAITFOR_BLACKBOX_DOCKER=1 to run against a real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker is required for Docker black-box test: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := docker(ctx, "info"); err != nil {
		t.Fatalf("docker daemon is required for Docker black-box test: %v", err)
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

	cmd := exec.CommandContext(ctx, waitforBinary, args...) // #nosec G204 -- black-box tests execute the just-built local binary with test-controlled args.
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

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms", "file", path, "--exists")
	requireExitCode(t, result, 0)
}

func TestBinaryFileStatesAndContains(t *testing.T) {
	requireBlackbox(t)

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready.log")
	if err := os.WriteFile(ready, []byte("service ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "file", ready, "--exists"), 0)
	requireExitCode(t, runWaitfor(t, "file", ready, "--nonempty"), 0)
	requireExitCode(t, runWaitfor(t, "file", ready, "--contains", "ready"), 0)
	requireExitCode(t, runWaitfor(t, "file", filepath.Join(dir, "missing"), "--deleted"), 0)
}

func TestBinaryTimeoutExitCode(t *testing.T) {
	requireBlackbox(t)

	missing := filepath.Join(t.TempDir(), "missing")
	result := runWaitfor(t, "--timeout", "75ms", "--interval", "20ms", "file", missing, "--exists")
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

func TestBinaryDNSLocalhost(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "dns", "localhost", "--type", "ANY", "--equals", "127.0.0.1", "--min-count", "1")
	requireExitCode(t, result, 0)
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
		"file", missing, "--exists",
		"--", "file", ready, "--exists"), 0)
	requireExitCode(t, runWaitfor(t, "--mode", "all", "--timeout", "100ms", "--interval", "25ms",
		"file", ready, "--exists",
		"--", "file", missing, "--exists"), 1)
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

	body, err := os.ReadFile(counter) // #nosec G304 -- counter path is created by this test in t.TempDir.
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
	result := runWaitfor(t, "--output", "json", "file", path, "--exists")
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

func TestBinaryConditionNamesAndBackoff(t *testing.T) {
	requireBlackbox(t)

	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runWaitfor(t,
		"--output", "json",
		"--interval", "10ms",
		"--backoff", "exponential",
		"--max-interval", "40ms",
		"--jitter", "0%",
		"file", path, "--exists", "--name", "ready-file")
	requireExitCode(t, result, 0)

	var payload struct {
		Backoff    string `json:"backoff"`
		Conditions []struct {
			Name string `json:"name"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, result.stdout)
	}
	if payload.Backoff != "exponential" || len(payload.Conditions) != 1 || payload.Conditions[0].Name != "ready-file" {
		t.Fatalf("payload = %+v, want named exponential condition", payload)
	}
}

func TestBinaryDoctorJSON(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "doctor", "--output", "json")
	requireExitCode(t, result, 0)
	if result.stderr != "" {
		t.Fatalf("stderr = %q, want empty doctor JSON stderr", result.stderr)
	}
	var payload struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, result.stdout)
	}
	if payload.Status == "" || len(payload.Checks) == 0 {
		t.Fatalf("payload = %+v, want doctor checks", payload)
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
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `"$1" --output json file "$2" --exists`, "sh", waitforBinary, path) // #nosec G204 -- test verifies shell quoting with test-controlled arguments.
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

func TestBinaryDockerContainerStatusAndHealthPolling(t *testing.T) {
	requireDockerBlackbox(t)

	name := fmt.Sprintf("waitfor-blackbox-%d", time.Now().UnixNano())
	cleanupDockerContainer(t, name)
	if err := docker(t.Context(),
		"run",
		"--detach",
		"--name", name,
		"--health-cmd", "test -f /tmp/ready",
		"--health-interval", "1s",
		"--health-retries", "30",
		"busybox:1.36",
		"sh", "-c", "sleep 60"); err != nil {
		t.Fatalf("docker run: %v", err)
	}

	readyErr := dockerExecAfter(500*time.Millisecond, name, "sh", "-c", "touch /tmp/ready")
	result := runWaitfor(t,
		"--timeout", "20s",
		"--interval", "200ms",
		"docker", name,
		"--status", "running",
		"--health", "healthy")
	requireExitCode(t, result, 0)
	requireAsyncDocker(t, readyErr, "mark container healthy")
}

func TestBinaryKubernetesNamespacePolling(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := fmt.Sprintf("waitfor-blackbox-%d", time.Now().UnixNano())
	cleanupNamespace(t, ns)

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
	args = appendKubeconfig(args)
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	if err := <-createErr; err != nil {
		t.Fatalf("kubectl create namespace: %v", err)
	}
}

func TestBinaryKubernetesNamespaceTimeout(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := fmt.Sprintf("waitfor-blackbox-missing-%d", time.Now().UnixNano())
	args := []string{
		"--timeout", "1s",
		"--interval", "200ms",
		"k8s", "namespace/" + ns,
		"--jsonpath", ".metadata.name == " + ns,
	}
	args = appendKubeconfig(args)
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 1)
}

func TestBinaryKubernetesPodReadyConditionPolling(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	pod := "waitfor-ready"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  containers:
    - name: pause
      image: registry.k8s.io/pause:3.10
`, pod, ns))

	patchErr := patchKubernetesStatusAfter(300*time.Millisecond,
		"pod", pod, ns,
		`{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`)

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "pod/" + pod,
		"--namespace", ns,
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	requireAsyncKubectl(t, patchErr, "patch pod status")
}

func TestBinaryKubernetesDeploymentAvailableConditionPolling(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	deployment := "waitfor-api"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
`, deployment, ns, deployment, deployment))

	patchErr := patchKubernetesStatusAfter(300*time.Millisecond,
		"deployment", deployment, ns,
		`{"status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"availableReplicas":1,"conditions":[{"type":"Available","status":"True","reason":"MinimumReplicasAvailable"}]}}`)

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "deployment/" + deployment,
		"--namespace", ns,
		"--condition", "Available",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	requireAsyncKubectl(t, patchErr, "patch deployment status")
}

func TestBinaryKubernetesServiceJSONPath(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	service := "waitfor-svc"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: waitfor
  ports:
    - port: 80
      targetPort: 8080
`, service, ns))

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "service/" + service,
		"--namespace", ns,
		"--jsonpath", ".spec.type == ClusterIP",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
}

func TestBinaryKubernetesMultipleResourcesPolling(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	service := "waitfor-multi"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: waitfor
  ports:
    - port: 80
      targetPort: 8080
`, service, ns))

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "namespace/" + ns,
		"--jsonpath", ".metadata.name == " + ns,
		"--",
		"k8s", "service/" + service,
		"--namespace", ns,
		"--jsonpath", ".metadata.name == " + service,
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
}

func TestBinaryKubernetesJobCompleteConditionPolling(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	job := "waitfor-migrate"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: registry.k8s.io/e2e-test-images/busybox:1.29-4
          command: ["sh", "-c", "true"]
`, job, ns))

	args := appendKubeconfig([]string{
		"--timeout", "30s",
		"--interval", "200ms",
		"k8s", "job/" + job,
		"--namespace", ns,
		"--condition", "Complete",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
}

func TestBinaryKubernetesStatefulSetJSONPath(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	statefulSet := "waitfor-db"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  serviceName: %s
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
`, statefulSet, ns, statefulSet, statefulSet, statefulSet))

	patchErr := patchKubernetesStatusAfter(300*time.Millisecond,
		"statefulset", statefulSet, ns,
		`{"status":{"replicas":1,"readyReplicas":1,"currentReplicas":1,"updatedReplicas":1}}`)

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "statefulset/" + statefulSet,
		"--namespace", ns,
		"--jsonpath", ".status.readyReplicas == 1",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	requireAsyncKubectl(t, patchErr, "patch statefulset status")
}

func TestBinaryKubernetesDaemonSetJSONPath(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	daemonSet := "waitfor-agent"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
`, daemonSet, ns, daemonSet, daemonSet))

	patchErr := patchKubernetesStatusAfter(300*time.Millisecond,
		"daemonset", daemonSet, ns,
		`{"status":{"desiredNumberScheduled":1,"currentNumberScheduled":1,"numberReady":1}}`)

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "daemonset/" + daemonSet,
		"--namespace", ns,
		"--jsonpath", ".status.numberReady >= 1",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	requireAsyncKubectl(t, patchErr, "patch daemonset status")
}

func TestBinaryKubernetesNamespaceFlagSelectsResource(t *testing.T) {
	requireKubernetesBlackbox(t)

	firstNS := createNamespace(t)
	secondNS := createNamespace(t)
	service := "waitfor-same-name"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: waitfor
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: waitfor
  ports:
    - port: 80
      targetPort: 8080
`, service, firstNS, service, secondNS))

	args := appendKubeconfig([]string{
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "svc/" + service,
		"--namespace", secondNS,
		"--jsonpath", ".metadata.namespace == " + secondNS,
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
}

func TestBinaryKubernetesUnsupportedKindFatal(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "k8s", "configmap/example")
	requireExitCode(t, result, 3)
}

func TestBinaryKubernetesJSONOutput(t *testing.T) {
	requireKubernetesBlackbox(t)

	ns := createNamespace(t)
	service := "waitfor-json"
	applyKubernetesYAML(t, fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: waitfor
  ports:
    - port: 80
      targetPort: 8080
`, service, ns))

	args := appendKubeconfig([]string{
		"--output", "json",
		"--timeout", "10s",
		"--interval", "200ms",
		"k8s", "service/" + service,
		"--namespace", ns,
		"--jsonpath", ".spec.type == ClusterIP",
	})
	result := runWaitfor(t, args...)
	requireExitCode(t, result, 0)
	if result.stderr != "" {
		t.Fatalf("stderr = %q, want empty for JSON output", result.stderr)
	}

	var payload struct {
		Status     string `json:"status"`
		Satisfied  bool   `json:"satisfied"`
		Conditions []struct {
			Backend   string `json:"backend"`
			Target    string `json:"target"`
			Satisfied bool   `json:"satisfied"`
		} `json:"conditions"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, result.stdout)
	}
	if payload.Status != "satisfied" || !payload.Satisfied {
		t.Fatalf("payload = %+v, want satisfied", payload)
	}
	if len(payload.Conditions) != 1 {
		t.Fatalf("conditions = %d, want 1", len(payload.Conditions))
	}
	condition := payload.Conditions[0]
	if condition.Backend != "k8s" || condition.Target != "service/"+service || !condition.Satisfied {
		t.Fatalf("condition = %+v, want satisfied k8s service", condition)
	}
}

func createNamespace(t *testing.T) string {
	t.Helper()
	ns := fmt.Sprintf("waitfor-blackbox-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := kubectl(ctx, "create", "namespace", ns); err != nil {
		t.Fatalf("kubectl create namespace: %v", err)
	}
	cleanupNamespace(t, ns)
	return ns
}

func cleanupNamespace(t *testing.T, ns string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found=true")
	})
}

func appendKubeconfig(args []string) []string {
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return append(args, "--kubeconfig", kubeconfig)
	}
	return args
}

func cleanupDockerContainer(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = docker(ctx, "rm", "--force", name)
	})
}

func dockerExecAfter(delay time.Duration, name string, args ...string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		execArgs := append([]string{"exec", name}, args...)
		errCh <- docker(ctx, execArgs...)
	}()
	return errCh
}

func requireAsyncDocker(t *testing.T, errCh <-chan error, action string) {
	t.Helper()
	if err := <-errCh; err != nil {
		t.Fatalf("docker %s: %v", action, err)
	}
}

func docker(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- integration helper intentionally drives Docker CLI with test-controlled args.
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, output.String())
	}
	return nil
}

func applyKubernetesYAML(t *testing.T, manifest string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := kubectlWithInput(ctx, manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("kubectl apply: %v", err)
	}
}

func patchKubernetesStatusAfter(delay time.Duration, kind string, name string, namespace string, patch string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		errCh <- kubectl(ctx,
			"patch", kind, name,
			"--namespace", namespace,
			"--subresource", "status",
			"--type", "merge",
			"--patch", patch)
	}()
	return errCh
}

func requireAsyncKubectl(t *testing.T, errCh <-chan error, action string) {
	t.Helper()
	if err := <-errCh; err != nil {
		t.Fatalf("kubectl %s: %v", action, err)
	}
}

func kubectl(ctx context.Context, args ...string) error {
	return kubectlWithInput(ctx, "", args...)
}

func kubectlWithInput(ctx context.Context, input string, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...) // #nosec G204 -- integration helper intentionally drives kubectl with test-controlled args.
	var output bytes.Buffer
	if input != "" {
		cmd.Stdin = bytes.NewBufferString(input)
	}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, output.String())
	}
	return nil
}
