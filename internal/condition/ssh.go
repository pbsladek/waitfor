package condition

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	maxSSHBannerLines = 20
	maxSSHBannerBytes = 1024
)

type SSHCondition struct {
	Address        string
	User           string
	Password       string
	BannerContains string
	HostKeySHA256  string
	Dial           func(context.Context, string) (net.Conn, error)
}

func NewSSH(address string) *SSHCondition {
	return &SSHCondition{Address: address}
}

func (c *SSHCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "ssh", Target: c.Address}
}

func (c *SSHCondition) Check(ctx context.Context) Result {
	if err := validateSSHConfig(c); err != nil {
		return Fatal(err)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	defer func() { _ = conn.Close() }()
	stopCancelWatcher := closeOnContextDone(ctx, conn)
	defer stopCancelWatcher()
	applySSHDeadline(ctx, conn)
	if c.authMode() {
		return c.checkAuth(conn)
	}
	return c.checkBanner(ctx, conn)
}

func validateSSHConfig(c *SSHCondition) error {
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("ssh address is required")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("invalid ssh address %q: %w", c.Address, err)
	}
	if (c.User == "") != (c.Password == "") {
		return fmt.Errorf("ssh --user and --password must be provided together")
	}
	if c.User != "" && c.HostKeySHA256 == "" {
		return fmt.Errorf("ssh password auth requires --host-key-sha256")
	}
	if c.HostKeySHA256 != "" && !validSSHHostKeySHA256(c.HostKeySHA256) {
		return fmt.Errorf("invalid ssh host key SHA256 fingerprint")
	}
	return nil
}

func validSSHHostKeySHA256(value string) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "SHA256:")
	_, err := base64.RawStdEncoding.DecodeString(value)
	return err == nil && value != ""
}

func (c *SSHCondition) authMode() bool {
	return c.User != "" && c.Password != ""
}

func (c *SSHCondition) dial(ctx context.Context) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, c.Address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", c.Address)
}

func applySSHDeadline(ctx context.Context, conn net.Conn) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
}

func (c *SSHCondition) checkBanner(ctx context.Context, conn net.Conn) Result {
	banner, err := readSSHBanner(ctx, conn)
	if err != nil {
		return Unsatisfied("", err)
	}
	if c.BannerContains != "" && !strings.Contains(banner, c.BannerContains) {
		detail := "ssh banner substring not found"
		return Unsatisfied(detail, errors.New(detail))
	}
	return Satisfied("banner " + banner)
}

func readSSHBanner(ctx context.Context, conn net.Conn) (string, error) {
	reader := bufio.NewReaderSize(conn, maxSSHBannerBytes)
	for i := 0; i < maxSSHBannerLines; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read ssh banner: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if validSSHBanner(line) {
			return line, nil
		}
	}
	return "", fmt.Errorf("ssh banner not found")
}

func validSSHBanner(line string) bool {
	if len(line) > maxSSHBannerBytes {
		return false
	}
	if !strings.HasPrefix(line, "SSH-2.0-") && !strings.HasPrefix(line, "SSH-1.99-") {
		return false
	}
	for i := 0; i < len(line); i++ {
		if line[i] < 0x20 || line[i] > 0x7e {
			return false
		}
	}
	return true
}

func (c *SSHCondition) checkAuth(conn net.Conn) Result {
	cfg := &ssh.ClientConfig{
		User:            c.User,
		Auth:            []ssh.AuthMethod{ssh.Password(c.Password)},
		HostKeyCallback: c.hostKeyCallback(),
		ClientVersion:   "SSH-2.0-waitfor",
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, c.Address, cfg)
	if err != nil {
		return Unsatisfied("", err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()
	return Satisfied("auth handshake succeeded")
}

func (c *SSHCondition) hostKeyCallback() ssh.HostKeyCallback {
	if c.HostKeySHA256 == "" {
		return func(_ string, _ net.Addr, _ ssh.PublicKey) error {
			return fmt.Errorf("ssh host key fingerprint is required")
		}
	}
	want := normalizeSSHHostKeySHA256(c.HostKeySHA256)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := sshHostKeySHA256(key)
		if got != want {
			return fmt.Errorf("ssh host key fingerprint %s did not match %s", got, want)
		}
		return nil
	}
}

func normalizeSSHHostKeySHA256(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "SHA256:") {
		return value
	}
	return "SHA256:" + value
}

func sshHostKeySHA256(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
