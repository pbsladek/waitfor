package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for Sec-WebSocket-Accept.
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pbsladek/wait-for/internal/cli"
	"github.com/pbsladek/wait-for/internal/output"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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

func newUnixSocketListener(t *testing.T) string {
	t.Helper()
	skipIfUnixSocketsUnsupported(t)
	path := filepath.Join(t.TempDir(), "ready.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets are not supported: %v", err)
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
	return path
}

func skipIfUnixSocketsUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not supported on windows")
	}
}

func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sh")
	}
}

func writeFakeExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- test helper creates a private executable under t.TempDir().
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeServerCertificatePEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server-cert.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newSSHBannerListener(t *testing.T, banner string) string {
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

func newSSHAuthListener(t *testing.T, user, password string) (string, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if conn.User() == user && string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected")
		},
	}
	config.AddHostKey(signer)
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
			go handleE2ESSHAuth(conn, config)
		}
	}()
	return listener.Addr().String(), e2eSSHHostKeySHA256(signer.PublicKey())
}

func handleE2ESSHAuth(conn net.Conn, config *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		_ = ch.Reject(ssh.Prohibited, "sessions are not supported")
	}
}

func e2eSSHHostKeySHA256(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
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
		"http", "http://"+refusedAddr(t))
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

// ── Unix Socket ─────────────────────────────────────────────────────────────

func TestUnixSocketSatisfied(t *testing.T) {
	path := newUnixSocketListener(t)
	mustCode(t, cli.ExitSatisfied, "unix", path)
}

func TestUnixSocketMissingTimeout(t *testing.T) {
	skipIfUnixSocketsUnsupported(t)
	path := filepath.Join(t.TempDir(), "missing.sock")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms", "unix", path)
}

func TestUnixSocketInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "unix")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "unix requires") {
		t.Fatalf("stderr %q does not mention unix parse error", stderr)
	}
}

// ── Ports ───────────────────────────────────────────────────────────────────

func TestPortsAnySatisfied(t *testing.T) {
	addr := newTCPListener(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "ports", host, "--range", port+"-"+port, "--any")
}

func TestPortsAllTimeout(t *testing.T) {
	addr := refusedAddr(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"ports", host, "--range", port+"-"+port, "--all")
}

func TestPortsInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "ports", "localhost", "--range", "8010-8000", "--any")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid ports range") {
		t.Fatalf("stderr %q does not mention invalid ports range", stderr)
	}
}

// ── SSH ─────────────────────────────────────────────────────────────────────

func TestSSHBannerSatisfied(t *testing.T) {
	addr := newSSHBannerListener(t, "SSH-2.0-e2e-ssh\r\n")
	mustCode(t, cli.ExitSatisfied, "ssh", addr)
}

func TestSSHBannerContainsTimeout(t *testing.T) {
	addr := newSSHBannerListener(t, "SSH-2.0-OpenSSH_9.9\r\n")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"ssh", addr, "--banner-contains", "dropbear")
}

func TestSSHAuthSatisfied(t *testing.T) {
	addr, fingerprint := newSSHAuthListener(t, "deploy", "secret")
	mustCode(t, cli.ExitSatisfied,
		"ssh", addr,
		"--user", "deploy",
		"--password", "secret",
		"--host-key-sha256", fingerprint)
}

func TestSSHInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "ssh", "api.example.com", "--user", "deploy")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid ssh address") {
		t.Fatalf("stderr %q does not mention invalid ssh address", stderr)
	}
}

// ── TLS ─────────────────────────────────────────────────────────────────────

func TestTLSSatisfied(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caPath := writeServerCertificatePEM(t, server)
	mustCode(t, cli.ExitSatisfied,
		"tls", server.Listener.Addr().String(),
		"--servername", "example.com",
		"--ca-file", caPath,
		"--valid-for", "24h")
}

func TestTLSExpiryWindowTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caPath := writeServerCertificatePEM(t, server)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"tls", server.Listener.Addr().String(),
		"--servername", "example.com",
		"--ca-file", caPath,
		"--valid-for", "1000000h")
}

func TestTLSSANMismatchTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caPath := writeServerCertificatePEM(t, server)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"tls", server.Listener.Addr().String(),
		"--servername", "definitely.invalid",
		"--ca-file", caPath)
}

func TestTLSUntrustedChainTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"tls", server.Listener.Addr().String(),
		"--servername", "example.com")
}

func TestTLSInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "tls", "api.example.com", "--valid-for", "30d")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid tls address") {
		t.Fatalf("stderr %q does not mention invalid tls address", stderr)
	}
}

// ── S3 ──────────────────────────────────────────────────────────────────────

func TestS3BucketExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/ready-bucket" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mustCode(t, cli.ExitSatisfied,
		"s3", "s3://ready-bucket",
		"--exists",
		"--endpoint-url", server.URL)
}

func TestS3ObjectMetadataAndContentSatisfied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ready-bucket/path/ready.json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("x-amz-meta-version", "42")
		_, _ = fmt.Fprint(w, `{"ready":true}`)
	}))
	defer server.Close()

	mustCode(t, cli.ExitSatisfied,
		"s3", "s3://ready-bucket/path/ready.json",
		"--endpoint-url", server.URL,
		"--metadata", "version=42",
		"--contains", `"ready":true`)
}

func TestS3CephRGWEndpointFromEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/ceph/ready-bucket/path/ready.json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("AWS_ENDPOINT_URL_S3", server.URL+"/ceph")

	mustCode(t, cli.ExitSatisfied,
		"s3", "s3://ready-bucket/path/ready.json",
		"--exists",
		"--region", "default")
}

func TestS3ObjectMissingTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"s3", "s3://ready-bucket/path/ready.json",
		"--endpoint-url", server.URL,
		"--exists")
}

func TestS3InvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "s3", "https://example.test/object", "--exists")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "invalid s3 URL") {
		t.Fatalf("stderr %q does not mention invalid s3 URL", stderr)
	}
}

func TestS3FatalConfig(t *testing.T) {
	mustCode(t, cli.ExitFatal,
		"s3", "s3://ready-bucket/path/ready.json",
		"--access-key-id", "only",
		"--secret-access-key", "")
}

// ── Glob ────────────────────────────────────────────────────────────────────

func TestGlobMinCountSatisfied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.done"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.done"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitSatisfied, "glob", filepath.Join(dir, "*.done"), "--min-count", "2")
}

func TestGlobAbsentSatisfied(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "glob", filepath.Join(t.TempDir(), "*.done"), "--absent")
}

func TestGlobMinCountTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.done"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"glob", filepath.Join(dir, "*.done"), "--min-count", "2")
}

func TestGlobInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "glob", "*.done", "--min-count", "2", "--max-count", "1")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "cannot exceed") {
		t.Fatalf("stderr %q does not mention min/max conflict", stderr)
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

func TestFileDeletedCannotContain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	_, stderr := mustCode(t, cli.ExitInvalid, "file", path, "--deleted", "--contains", "ready")
	if !strings.Contains(stderr, "--deleted cannot be combined") {
		t.Fatalf("stderr %q does not mention --deleted conflict", stderr)
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

func TestLogMatchedLineJSONDetailDoesNotExposeLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service ready at port 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ := mustCode(t, cli.ExitSatisfied, "--output", "json",
		"log", path, "--contains", "ready", "--from-start")
	if strings.Contains(stdout, "port 8080") || !strings.Contains(stdout, "matched line") {
		t.Fatalf("JSON output %q should contain generic detail only", stdout)
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
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec", "--", "/bin/sh", "-c", "exit 0")
}

func TestExecExpectedExitCode(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec", "--exit-code", "7", "--", "/bin/sh", "-c", "exit 7")
}

func TestExecExitCodeMismatch(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--", "/bin/sh", "-c", "exit 1")
}

func TestExecOutputContains(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec", "--output-contains", "hello", "--", "/bin/sh", "-c", "echo hello")
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--output-contains", "missing", "--", "/bin/sh", "-c", "echo hello")
}

func TestExecJSONPath(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec", "--jsonpath", ".ready == true",
		"--", "/bin/sh", "-c", `printf '{"ready":true}'`)
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"exec", "--jsonpath", ".ready == true",
		"--", "/bin/sh", "-c", `printf '{"ready":false}'`)
}

func TestExecCwd(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	mustCode(t, cli.ExitSatisfied, "exec", "--cwd", dir, "--output-contains", dir,
		"--", "/bin/sh", "-c", "pwd")
}

func TestExecEnv(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec",
		"--env", "WAITFOR_E2E=yep",
		"--output-contains", "yep",
		"--", "/bin/sh", "-c", "echo $WAITFOR_E2E")
}

func TestExecMaxOutputBytes(t *testing.T) {
	requirePOSIXShell(t)
	mustCode(t, cli.ExitSatisfied, "exec",
		"--max-output-bytes", "3",
		"--output-contains", "abc",
		"--", "/bin/sh", "-c", "printf abcdefgh")
}

func TestExecCommandNotFound(t *testing.T) {
	mustCode(t, cli.ExitFatal, "exec", "--", "/definitely/no/such/command")
}

func TestExecTimeout(t *testing.T) {
	requirePOSIXShell(t)
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
		"--", "tcp", refusedAddr(t))
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
	mustCode(t, cli.ExitSatisfied, "dns", "localhost", "--type", "ANY", "--min-count", "1")
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

func TestProcessPIDSatisfied(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "process", "--pid", fmt.Sprint(os.Getpid()), "--running")
}

func TestProcessNameSatisfied(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "ps", "#!/bin/sh\nprintf '123 postgres postgres -D data\\n'\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitSatisfied, "process", "--name", "postgres", "--running")
}

func TestProcessStoppedTimeout(t *testing.T) {
	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"process", "--pid", fmt.Sprint(os.Getpid()), "--stopped")
}

func TestProcessInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "process", "--running")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "exactly one") {
		t.Fatalf("stderr %q does not mention selector requirement", stderr)
	}
}

func TestProcessMissingPSFatal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	mustCode(t, cli.ExitFatal, "process", "--name", "postgres", "--running")
}

func TestSystemdActiveSatisfied(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "systemctl", "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitSatisfied, "systemd", "nginx.service", "--active")
}

func TestSystemdActiveTimeout(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "systemctl", "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=inactive\\nSubState=dead\\n'\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitTimeout, "--timeout", "50ms", "--interval", "10ms",
		"systemd", "nginx.service", "--active")
}

func TestSystemdInvalidArgs(t *testing.T) {
	code, _, stderr := execute(t, "systemd", "nginx.service", "--active", "--failed")
	if code != cli.ExitInvalid {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitInvalid)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr %q does not mention mutually exclusive", stderr)
	}
}

func TestSystemdMissingSystemctlFatal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	mustCode(t, cli.ExitFatal, "systemd", "nginx.service", "--active")
}

func TestLaunchdRunningSatisfied(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "launchctl", "#!/bin/sh\nprintf 'pid = 123\\nstate = running\\n'\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitSatisfied, "launchd", "system/com.example.agent", "--running")
}

func TestPIDFileRunningSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pid")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	mustCode(t, cli.ExitSatisfied, "pidfile", path, "--running")
}

func TestLockfileAbsentSatisfied(t *testing.T) {
	mustCode(t, cli.ExitSatisfied, "lockfile", filepath.Join(t.TempDir(), "app.lock"), "--absent")
}

func TestLockfileOlderThanSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	mustCode(t, cli.ExitSatisfied, "lockfile", path, "--older-than", "30m")
}

func TestPermissionModeSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G302 -- test needs to assert mode matching above 0600.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	mustCode(t, cli.ExitSatisfied, "permission", path, "--mode", "0640", "--type", "file")
}

func TestChecksumSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	mustCode(t, cli.ExitSatisfied, "checksum", path, "--equals", "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
}

func TestArchiveContainsSatisfied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.zip")
	writeE2EZipArchive(t, path, "bin/app")

	mustCode(t, cli.ExitSatisfied, "archive", path, "--matches", "bin/*")
}

func TestCosignBlobSatisfied(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "cosign", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitSatisfied, "cosign", "--blob", filepath.Join(dir, "artifact"), "--signature", filepath.Join(dir, "artifact.sig"), "--certificate-identity", "id", "--certificate-oidc-issuer", "issuer")
}

func TestICMPSatisfiedWithFakePing(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "ping", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", dir)

	mustCode(t, cli.ExitSatisfied, "icmp", "127.0.0.1", "--count", "2", "--timeout", "500ms")
}

func TestNTPSatisfied(t *testing.T) {
	addr := newNTPServer(t)

	mustCode(t, cli.ExitSatisfied, "ntp", addr, "--max-offset", "5s", "--timeout", "500ms")
}

func TestGRPCHealthSatisfied(t *testing.T) {
	addr := newGRPCHealthServer(t)

	mustCode(t, cli.ExitSatisfied, "grpc", addr, "--service", "svc")
}

func TestWebSocketSatisfied(t *testing.T) {
	url := newWebSocketServer(t, "ready")

	mustCode(t, cli.ExitSatisfied, "websocket", url, "--send", "hello", "--matches", "rea.*", "--header", "Authorization=Bearer token", "--timeout", "500ms")
}

func TestExtraBackendsInvalidArgs(t *testing.T) {
	tests := [][]string{
		{"launchd"},
		{"pidfile"},
		{"lockfile"},
		{"permission"},
		{"checksum"},
		{"archive"},
		{"cosign"},
		{"ntp"},
		{"icmp"},
		{"grpc"},
		{"websocket"},
	}
	for _, args := range tests {
		code, _, _ := execute(t, args...)
		if code != cli.ExitInvalid {
			t.Fatalf("execute(%v) code = %d, want %d", args, code, cli.ExitInvalid)
		}
	}
}

func TestExtraBackendsTimeouts(t *testing.T) {
	requirePOSIXShell(t)
	dir := t.TempDir()
	writeFakeExecutable(t, dir, "launchctl", "#!/bin/sh\nprintf 'state = waiting\\n'\n")
	writeFakeExecutable(t, dir, "cosign", "#!/bin/sh\nexit 1\n")
	writeFakeExecutable(t, dir, "ping", "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", dir)
	mismatchFile := filepath.Join(dir, "mismatch")
	if err := os.WriteFile(mismatchFile, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "app.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "app.zip")
	writeE2EZipArchive(t, archivePath, "bin/app")
	tests := [][]string{
		{"--timeout", "40ms", "--interval", "10ms", "launchd", "system/com.example.agent", "--running"},
		{"--timeout", "40ms", "--interval", "10ms", "pidfile", filepath.Join(dir, "missing.pid"), "--running"},
		{"--timeout", "40ms", "--interval", "10ms", "lockfile", lock, "--absent"},
		{"--timeout", "40ms", "--interval", "10ms", "permission", mismatchFile, "--mode", "0640"},
		{"--timeout", "40ms", "--interval", "10ms", "checksum", mismatchFile, "--equals", strings.Repeat("0", 64)},
		{"--timeout", "40ms", "--interval", "10ms", "archive", archivePath, "--contains", "missing"},
		{"--timeout", "40ms", "--interval", "10ms", "cosign", "ghcr.io/example/app:latest"},
		{"--timeout", "40ms", "--interval", "10ms", "icmp", "127.0.0.1"},
		{"--timeout", "40ms", "--interval", "10ms", "ntp", newNTPServerWithOffset(t, time.Minute), "--max-offset", "1ms"},
		{"--timeout", "40ms", "--interval", "10ms", "grpc", newGRPCHealthServerWithStatus(t, 2)},
		{"--timeout", "40ms", "--interval", "10ms", "websocket", newWebSocketServer(t, "cold"), "--send", "hello", "--contains", "ready"},
	}
	for _, args := range tests {
		code, _, _ := execute(t, args...)
		if code != cli.ExitTimeout {
			t.Fatalf("execute(%v) code = %d, want %d", args, code, cli.ExitTimeout)
		}
	}
}

func TestExtraBackendsFatal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tests := [][]string{
		{"launchd", "system/com.example.agent"},
		{"cosign", "ghcr.io/example/app:latest"},
		{"icmp", "127.0.0.1"},
		{"permission", filepath.Join(t.TempDir(), "missing")},
		{"checksum", filepath.Join(t.TempDir(), "missing")},
		{"archive", filepath.Join(t.TempDir(), "missing")},
		{"grpc", "127.0.0.1:1", "--status", "BROKEN"},
	}
	for _, args := range tests {
		code, _, _ := execute(t, args...)
		if code != cli.ExitFatal {
			t.Fatalf("execute(%v) code = %d, want %d", args, code, cli.ExitFatal)
		}
	}
}

func writeE2EZipArchive(t *testing.T, path, name string) {
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

func newNTPServer(t *testing.T) string {
	return newNTPServerWithOffset(t, 0)
}

func newNTPServerWithOffset(t *testing.T, offset time.Duration) string {
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
		writeE2ENTPTimestamp(resp[32:40], time.Now().Add(offset))
		writeE2ENTPTimestamp(resp[40:48], time.Now().Add(offset))
		_, _ = conn.WriteTo(resp, addr)
	}()
	return conn.LocalAddr().String()
}

func writeE2ENTPTimestamp(dst []byte, t time.Time) {
	seconds := e2eClampInt64ToUint32(t.Unix() + 2208988800)
	fraction := uint64(float64(t.Nanosecond()) * (1 << 32) / 1e9)
	binary.BigEndian.PutUint32(dst[0:4], seconds)
	binary.BigEndian.PutUint32(dst[4:8], e2eClampUint64ToUint32(fraction))
}

func newGRPCHealthServer(t *testing.T) string {
	return newGRPCHealthServerWithStatus(t, 1)
}

func newGRPCHealthServerWithStatus(t *testing.T, status byte) string {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/grpc.health.v1.Health/Check" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		_, _ = w.Write(grpcE2EFrame([]byte{0x08, status}))
		w.Header().Set("Grpc-Status", "0")
	})
	server := httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
	t.Cleanup(server.Close)
	return "grpc://" + strings.TrimPrefix(server.URL, "http://")
}

func grpcE2EFrame(payload []byte) []byte {
	frame := make([]byte, 5, len(payload)+5)
	binary.BigEndian.PutUint32(frame[1:5], e2eUint32Length(len(payload)))
	return append(frame, payload...)
}

func newWebSocketServer(t *testing.T, message string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		accept := websocketE2EAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		_ = rw.Flush()
		_, _, _ = readE2EWebSocketFrame(rw)
		_, _ = rw.Write(websocketE2ETextFrame(message))
		_ = rw.Flush()
	}))
	t.Cleanup(server.Close)
	return "ws://" + strings.TrimPrefix(server.URL, "http://")
}

func websocketE2EAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) // #nosec G401 -- RFC 6455 requires SHA-1.
	return base64.StdEncoding.EncodeToString(sum[:])
}

func websocketE2ETextFrame(message string) []byte {
	payload := []byte(message)
	frame := []byte{0x81, e2eByteLength(len(payload))}
	return append(frame, payload...)
}

func readE2EWebSocketFrame(r io.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	var mask [4]byte
	if header[1]&0x80 != 0 {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
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

func e2eClampInt64ToUint32(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func e2eClampUint64ToUint32(value uint64) uint32 {
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func e2eUint32Length(length int) uint32 {
	if length < 0 {
		return 0
	}
	if length > int(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(length)
}

func e2eByteLength(length int) byte {
	if length < 0 {
		return 0
	}
	if length > 125 {
		return 125
	}
	return byte(length)
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
