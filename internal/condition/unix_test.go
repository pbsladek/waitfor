package condition

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestUnixConditionSatisfied(t *testing.T) {
	path := newUnixSocketListener(t)

	result := NewUnix(path).Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, want satisfied, err = %v", result.Status, result.Err)
	}
	if result.Detail != "connection established" {
		t.Fatalf("detail = %q, want connection established", result.Detail)
	}
}

func TestUnixConditionMissingSocketUnsatisfied(t *testing.T) {
	skipIfUnixSocketsUnsupported(t)

	result := NewUnix(filepath.Join(t.TempDir(), "missing.sock")).Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Err == nil {
		t.Fatal("expected dial error")
	}
}

func TestUnixConditionInvalidConfigFatal(t *testing.T) {
	for _, path := range []string{"", " \t "} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			result := NewUnix(path).Check(t.Context())
			if result.Status != CheckFatal {
				t.Fatalf("status = %s, want fatal", result.Status)
			}
		})
	}
}

func TestUnixConditionDescriptor(t *testing.T) {
	desc := NewUnix("/var/run/docker.sock").Descriptor()
	if desc.Backend != "unix" || desc.Target != "/var/run/docker.sock" {
		t.Fatalf("descriptor = %+v", desc)
	}
}

func TestUnixConditionInjectedDial(t *testing.T) {
	cond := NewUnix("/tmp/app.sock")
	cond.Dial = func(ctx context.Context, path string) (net.Conn, error) {
		if path != "/tmp/app.sock" {
			t.Fatalf("path = %q, want /tmp/app.sock", path)
		}
		return unixNoopConn{}, nil
	}

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, want satisfied", result.Status)
	}
}

func TestUnixConditionContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cond := NewUnix("/tmp/app.sock")
	cond.Dial = func(ctx context.Context, path string) (net.Conn, error) {
		return nil, ctx.Err()
	}

	result := cond.Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
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

type unixNoopConn struct{}

func (unixNoopConn) Read([]byte) (int, error)         { return 0, nil }
func (unixNoopConn) Write([]byte) (int, error)        { return 0, nil }
func (unixNoopConn) Close() error                     { return nil }
func (unixNoopConn) LocalAddr() net.Addr              { return nil }
func (unixNoopConn) RemoteAddr() net.Addr             { return nil }
func (unixNoopConn) SetDeadline(time.Time) error      { return nil }
func (unixNoopConn) SetReadDeadline(time.Time) error  { return nil }
func (unixNoopConn) SetWriteDeadline(time.Time) error { return nil }
