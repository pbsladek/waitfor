package condition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSSHBannerSatisfied(t *testing.T) {
	addr := startSSHBannerServer(t, "SSH-2.0-test-ssh\r\n")

	cond := NewSSH(addr)
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if !strings.Contains(result.Detail, "SSH-2.0-test-ssh") {
		t.Fatalf("detail = %q, want banner", result.Detail)
	}
}

func TestSSHBannerContains(t *testing.T) {
	addr := startSSHBannerServer(t, "SSH-2.0-OpenSSH_9.9\r\n")

	cond := NewSSH(addr)
	cond.BannerContains = "OpenSSH"
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestSSHBannerContainsMismatch(t *testing.T) {
	addr := startSSHBannerServer(t, "SSH-2.0-OpenSSH_9.9\r\n")

	cond := NewSSH(addr)
	cond.BannerContains = "dropbear"
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "ssh banner substring not found" {
		t.Fatalf("detail = %q, want substring detail", result.Detail)
	}
}

func TestSSHSkipsPreBannerLines(t *testing.T) {
	addr := startSSHBannerServer(t, "notice\r\nSSH-2.0-test-ssh\r\n")

	result := NewSSH(addr).Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
}

func TestSSHBannerReadIsBounded(t *testing.T) {
	reader := strings.NewReader(strings.Repeat("x", 10*maxSSHBannerBytes))
	size := reader.Len()
	if _, err := readSSHBanner(t.Context(), readOnlyConn{Reader: reader}); err == nil {
		t.Fatal("unterminated oversized banner was accepted")
	}
	if read := size - reader.Len(); read > maxSSHBannerBytes+2 {
		t.Fatalf("read %d bytes before rejecting oversized banner", read)
	}
	for _, ending := range []string{"\n", "\r\n"} {
		banner := "SSH-2.0-" + strings.Repeat("x", maxSSHBannerBytes-len("SSH-2.0-"))
		got, err := readSSHBanner(t.Context(), readOnlyConn{Reader: strings.NewReader(banner + ending)})
		if err != nil || got != banner {
			t.Fatalf("maximum banner with %q: got %q, err %v", ending, got, err)
		}
	}
	oversized := strings.Repeat("x", maxSSHBannerBytes+1) + "\n"
	if _, err := readSSHBanner(t.Context(), readOnlyConn{Reader: strings.NewReader(oversized)}); err == nil {
		t.Fatal("oversized terminated line was accepted")
	}
}

func TestSSHHostKeyFingerprintLength(t *testing.T) {
	for _, size := range []int{0, 1, 31, 32, 33} {
		value := base64.RawStdEncoding.EncodeToString(make([]byte, size))
		for _, prefix := range []string{"", "SHA256:"} {
			if got := validSSHHostKeySHA256(prefix + value); got != (size == 32) {
				t.Errorf("valid fingerprint size %d prefix %q = %t", size, prefix, got)
			}
		}
	}
}

func TestSSHAuthSatisfied(t *testing.T) {
	addr, fingerprint := startSSHAuthServer(t, "deploy", "secret")

	cond := NewSSH(addr)
	cond.User = "deploy"
	cond.Password = "secret"
	cond.HostKeySHA256 = fingerprint
	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "auth handshake succeeded" {
		t.Fatalf("detail = %q, want auth detail", result.Detail)
	}
}

func TestSSHAuthWrongPasswordUnsatisfied(t *testing.T) {
	addr, fingerprint := startSSHAuthServer(t, "deploy", "secret")

	cond := NewSSH(addr)
	cond.User = "deploy"
	cond.Password = "wrong"
	cond.HostKeySHA256 = fingerprint
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestSSHAuthHostKeyMismatchUnsatisfied(t *testing.T) {
	addr, _ := startSSHAuthServer(t, "deploy", "secret")

	cond := NewSSH(addr)
	cond.User = "deploy"
	cond.Password = "secret"
	cond.HostKeySHA256 = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestSSHInvalidDirectConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*SSHCondition)
	}{
		{"empty address", func(c *SSHCondition) { c.Address = "" }},
		{"bad address", func(c *SSHCondition) { c.Address = "example.test" }},
		{"partial auth", func(c *SSHCondition) { c.User = "deploy" }},
		{"auth without host key", func(c *SSHCondition) { c.User = "deploy"; c.Password = "secret" }},
		{"bad fingerprint", func(c *SSHCondition) { c.HostKeySHA256 = "not-base64" }},
		{"short fingerprint", func(c *SSHCondition) { c.HostKeySHA256 = "SHA256:YQ" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewSSH("127.0.0.1:22")
			cond.Dial = func(context.Context, string) (net.Conn, error) {
				t.Fatal("dial should not be called for invalid config")
				return nil, nil
			}
			tt.setup(cond)
			result := cond.Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestSSHDescriptor(t *testing.T) {
	desc := NewSSH("example.test:22").Descriptor()
	if desc.Backend != "ssh" || desc.Target != "example.test:22" {
		t.Fatalf("Descriptor() = %+v", desc)
	}
}

func TestSSHAdditionalHelperBranches(t *testing.T) {
	cond := NewSSH("127.0.0.1:22")
	cond.Dial = func(context.Context, string) (net.Conn, error) {
		return nil, fmt.Errorf("dial failed")
	}
	if result := cond.Check(t.Context()); result.Status != CheckUnsatisfied {
		t.Fatalf("dial failure status = %s, want unsatisfied", result.Status)
	}

	if validSSHBanner("SSH-2.0-bad\x01") {
		t.Fatal("banner with control character was valid")
	}
	if validSSHBanner(strings.Repeat("x", maxSSHBannerBytes+1)) {
		t.Fatal("oversized banner was valid")
	}
	if validSSHBanner("NOTICE") {
		t.Fatal("non-SSH banner was valid")
	}

	conn := readOnlyConn{Reader: strings.NewReader(strings.Repeat("notice\n", maxSSHBannerLines))}
	if _, err := readSSHBanner(t.Context(), conn); err == nil {
		t.Fatal("readSSHBanner without banner succeeded")
	}

	callback := (&SSHCondition{}).hostKeyCallback()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback("", nil, signer.PublicKey()); err == nil {
		t.Fatal("missing host key callback accepted key")
	}
	if got := normalizeSSHHostKeySHA256("abc"); got != "SHA256:abc" {
		t.Fatalf("normalizeSSHHostKeySHA256 = %q", got)
	}
}

func startSSHBannerServer(t *testing.T, banner string) string {
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

func startSSHAuthServer(t *testing.T, user, password string) (string, string) {
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
	go acceptSSHAuthConnections(listener, config)
	return listener.Addr().String(), sshHostKeySHA256(signer.PublicKey())
}

func acceptSSHAuthConnections(listener net.Listener, config *ssh.ServerConfig) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleSSHAuthConnection(conn, config)
	}
}

func handleSSHAuthConnection(conn net.Conn, config *ssh.ServerConfig) {
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
