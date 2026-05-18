package integration_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for Sec-WebSocket-Accept.
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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

func writeBlackboxServerCertificatePEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server-cert.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newBlackboxSSHBannerListener(t *testing.T, banner string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_, _ = fmt.Fprint(conn, banner)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func newBlackboxTCPListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

func skipIfBlackboxUnixSocketsUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not supported on windows")
	}
	path := filepath.Join(t.TempDir(), "probe.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets are not supported: %v", err)
	}
	_ = listener.Close()
}

func acceptBlackboxUnixConnections(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

func writeBlackboxExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- black-box helper creates a private executable under t.TempDir().
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeBlackboxZipArchive(t *testing.T, path, name string) {
	t.Helper()
	file, err := os.Create(path) // #nosec G304 -- test helper writes only caller-provided t.TempDir() paths.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := zip.NewWriter(file)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func newBlackboxNTPServer(t *testing.T, offset time.Duration) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 48)
		n, addr, err := conn.ReadFrom(buf)
		if err != nil || n < 48 {
			return
		}
		resp := make([]byte, 48)
		resp[0] = 0x24
		resp[1] = 1
		copy(resp[24:32], buf[40:48])
		writeBlackboxNTPTimestamp(resp[32:40], time.Now().Add(offset))
		writeBlackboxNTPTimestamp(resp[40:48], time.Now().Add(offset))
		_, _ = conn.WriteTo(resp, addr)
	}()
	return conn.LocalAddr().String()
}

func writeBlackboxNTPTimestamp(dst []byte, t time.Time) {
	seconds := clampBlackboxInt64ToUint32(t.Unix() + 2208988800)
	fraction := uint64(float64(t.Nanosecond()) * (1 << 32) / 1e9)
	binary.BigEndian.PutUint32(dst[0:4], seconds)
	binary.BigEndian.PutUint32(dst[4:8], clampBlackboxUint64ToUint32(fraction))
}

func newBlackboxGRPCHealthServer(t *testing.T, status byte) string {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grpc.health.v1.Health/Check" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		_, _ = w.Write(blackboxGRPCFrame([]byte{0x08, status}))
		w.Header().Set("Grpc-Status", "0")
	})
	server := httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
	t.Cleanup(server.Close)
	return "grpc://" + strings.TrimPrefix(server.URL, "http://")
}

func blackboxGRPCFrame(payload []byte) []byte {
	frame := make([]byte, 5, len(payload)+5)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload))) // #nosec G115 -- test payloads are tiny.
	return append(frame, payload...)
}

func newBlackboxWebSocketServer(t *testing.T, message string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			return
		}
		accept := blackboxWebSocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		_ = rw.Flush()
		_, payload, err := readBlackboxWebSocketClientFrame(rw)
		if err != nil {
			t.Errorf("client frame: %v", err)
			return
		}
		if string(payload) != "hello" {
			t.Errorf("payload = %q", payload)
			return
		}
		_, _ = rw.Write(blackboxWebSocketTextFrame(message))
		_ = rw.Flush()
	}))
	t.Cleanup(server.Close)
	return "ws://" + strings.TrimPrefix(server.URL, "http://")
}

func blackboxWebSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) // #nosec G401 -- RFC 6455 requires SHA-1.
	return base64.StdEncoding.EncodeToString(sum[:])
}

func blackboxWebSocketTextFrame(message string) []byte {
	payload := []byte(message)
	frame := []byte{0x81, byte(len(payload))} // #nosec G115 -- test messages are shorter than 126 bytes.
	return append(frame, payload...)
}

func readBlackboxWebSocketClientFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame was not masked")
	}
	length := int(header[1] & 0x7f)
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return header[0] & 0x0f, payload, nil
}

func clampBlackboxInt64ToUint32(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func clampBlackboxUint64ToUint32(value uint64) uint32 {
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
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

func TestBinaryGlobPolling(t *testing.T) {
	requireBlackbox(t)

	dir := t.TempDir()
	timer := time.AfterFunc(150*time.Millisecond, func() {
		_ = os.WriteFile(filepath.Join(dir, "one.done"), []byte("ready\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "two.done"), []byte("ready\n"), 0o600)
	})
	defer timer.Stop()

	pattern := filepath.Join(dir, "*.done")
	requireExitCode(t, runWaitfor(t, "--timeout", "2s", "--interval", "25ms",
		"glob", pattern, "--min-count", "2"), 0)
	requireExitCode(t, runWaitfor(t, "glob", filepath.Join(dir, "*.missing"), "--absent"), 0)
}

func TestBinaryTimeoutExitCode(t *testing.T) {
	requireBlackbox(t)

	missing := filepath.Join(t.TempDir(), "missing")
	result := runWaitfor(t, "--timeout", "75ms", "--interval", "20ms", "file", missing, "--exists")
	requireExitCode(t, result, 1)
}

func TestBinaryTLSPolling(t *testing.T) {
	requireBlackbox(t)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caPath := writeBlackboxServerCertificatePEM(t, server)
	requireExitCode(t, runWaitfor(t,
		"tls", server.Listener.Addr().String(),
		"--servername", "example.com",
		"--ca-file", caPath,
		"--valid-for", "24h"), 0)
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"tls", server.Listener.Addr().String(),
		"--servername", "example.com",
		"--ca-file", caPath,
		"--valid-for", "1000000h"), 1)
}

func TestBinarySSHPolling(t *testing.T) {
	requireBlackbox(t)

	addr := newBlackboxSSHBannerListener(t, "SSH-2.0-blackbox-ssh\r\n")
	requireExitCode(t, runWaitfor(t, "ssh", addr, "--banner-contains", "blackbox"), 0)
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"ssh", addr, "--banner-contains", "missing"), 1)
}

func TestBinaryS3Polling(t *testing.T) {
	requireBlackbox(t)

	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ready-bucket/path/ready.json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if attempts.Add(1) < 3 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("x-amz-meta-version", "42")
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()

	result := runWaitfor(t, "--timeout", "2s", "--interval", "25ms",
		"s3", "s3://ready-bucket/path/ready.json",
		"--endpoint-url", server.URL,
		"--metadata", "version=42",
		"--contains", `"ready":true`)
	requireExitCode(t, result, 0)
	if attempts.Load() < 3 {
		t.Fatalf("attempts = %d, want polling before success", attempts.Load())
	}
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"s3", "s3://ready-bucket/path/missing.json",
		"--endpoint-url", server.URL,
		"--exists"), 1)
}

func TestBinaryS3CephEndpointEnvironment(t *testing.T) {
	requireBlackbox(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/rgw/ready-bucket/path/ready.json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("AWS_ENDPOINT_URL_S3", server.URL+"/rgw")
	requireExitCode(t, runWaitfor(t,
		"s3", "s3://ready-bucket/path/ready.json",
		"--exists",
		"--region", "default"), 0)
}

func TestBinaryProcessPIDPolling(t *testing.T) {
	requireBlackbox(t)

	requireExitCode(t, runWaitfor(t, "process", "--pid", strconv.Itoa(os.Getpid()), "--running"), 0)
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"process", "--pid", strconv.Itoa(os.Getpid()), "--stopped"), 1)
}

func TestBinaryExtraLocalBackends(t *testing.T) {
	requireBlackbox(t)

	dir := t.TempDir()
	pidfile := filepath.Join(dir, "app.pid")
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "pidfile", pidfile, "--running"), 0)

	lockfile := filepath.Join(dir, "app.lock")
	if err := os.WriteFile(lockfile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockfile, old, old); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "lockfile", lockfile, "--older-than", "30m"), 0)

	permfile := filepath.Join(dir, "mode")
	if err := os.WriteFile(permfile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- black-box test validates non-0600 permission matching.
	if err := os.Chmod(permfile, 0o640); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "permission", permfile, "--mode", "0640", "--type", "file"), 0)

	checksum := filepath.Join(dir, "checksum")
	if err := os.WriteFile(checksum, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "checksum", checksum, "--equals", "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"), 0)

	archive := filepath.Join(dir, "app.zip")
	writeBlackboxZipArchive(t, archive, "bin/app")
	requireExitCode(t, runWaitfor(t, "archive", archive, "--matches", "bin/*"), 0)
}

func TestBinaryExtraCommandBackends(t *testing.T) {
	requireBlackbox(t)
	if runtime.GOOS == "windows" {
		t.Skip("fake command scripts require /bin/sh")
	}
	dir := t.TempDir()
	writeBlackboxExecutable(t, dir, "launchctl", "#!/bin/sh\nprintf 'pid = 123\\nstate = running\\n'\n")
	writeBlackboxExecutable(t, dir, "cosign", "#!/bin/sh\nexit 0\n")
	writeBlackboxExecutable(t, dir, "ping", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	requireExitCode(t, runWaitfor(t, "launchd", "system/com.example.agent", "--running"), 0)
	requireExitCode(t, runWaitfor(t, "cosign", "--blob", filepath.Join(dir, "artifact"), "--signature", filepath.Join(dir, "artifact.sig")), 0)
	requireExitCode(t, runWaitfor(t, "icmp", "127.0.0.1", "--count", "2", "--timeout", "500ms"), 0)
}

func TestBinaryExtraProtocolBackends(t *testing.T) {
	requireBlackbox(t)

	requireExitCode(t, runWaitfor(t, "ntp", newBlackboxNTPServer(t, 0), "--max-offset", "5s", "--timeout", "500ms"), 0)
	requireExitCode(t, runWaitfor(t, "grpc", newBlackboxGRPCHealthServer(t, 1), "--service", "svc"), 0)
	requireExitCode(t, runWaitfor(t, "websocket", newBlackboxWebSocketServer(t, "ready"), "--send", "hello", "--contains", "ready", "--header", "Authorization=Bearer token", "--timeout", "500ms"), 0)
}

func TestBinarySystemdInvalidArgs(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "systemd", "nginx.service", "--active", "--failed")
	requireExitCode(t, result, 2)
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

func TestBinaryUnixSocketPolling(t *testing.T) {
	requireBlackbox(t)
	skipIfBlackboxUnixSocketsUnsupported(t)

	path := filepath.Join(t.TempDir(), "ready.sock")
	listenerCh := make(chan net.Listener, 1)
	errCh := make(chan error, 1)
	timer := time.AfterFunc(150*time.Millisecond, func() {
		listener, err := net.Listen("unix", path)
		if err != nil {
			errCh <- err
			return
		}
		listenerCh <- listener
		go acceptBlackboxUnixConnections(listener)
	})
	defer timer.Stop()

	requireExitCode(t, runWaitfor(t, "--timeout", "2s", "--interval", "25ms", "unix", path), 0)

	select {
	case err := <-errCh:
		t.Fatalf("unix listener: %v", err)
	case listener := <-listenerCh:
		_ = listener.Close()
	default:
		t.Fatal("unix listener was not started")
	}
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"unix", filepath.Join(t.TempDir(), "missing.sock")), 1)
}

func TestBinaryPortsPolling(t *testing.T) {
	requireBlackbox(t)

	addr := newBlackboxTCPListener(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "ports", host, "--range", port+"-"+port, "--any"), 0)

	refused := freeTCPAddress(t)
	host, port, err = net.SplitHostPort(refused)
	if err != nil {
		t.Fatal(err)
	}
	requireExitCode(t, runWaitfor(t, "--timeout", "75ms", "--interval", "20ms",
		"ports", host, "--range", port+"-"+port, "--all"), 1)
}

func TestBinaryDNSLocalhost(t *testing.T) {
	requireBlackbox(t)

	result := runWaitfor(t, "dns", "localhost", "--type", "ANY", "--min-count", "1")
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
