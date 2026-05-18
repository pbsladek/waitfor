package condition

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
