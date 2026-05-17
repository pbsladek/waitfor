package condition

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPortsAnySatisfied(t *testing.T) {
	openPort := 8002
	cond := NewPorts("localhost", 8000, 8003)
	cond.Mode = PortsAny
	cond.Dial = fakePortsDial(map[int]bool{openPort: true})

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "port 8002 open" {
		t.Fatalf("detail = %q, want open port detail", result.Detail)
	}
}

func TestPortsAnyUnsatisfied(t *testing.T) {
	cond := NewPorts("localhost", 8000, 8001)
	cond.Mode = PortsAny
	cond.Dial = fakePortsDial(nil)

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "no open ports in 8000-8001" {
		t.Fatalf("detail = %q, want no open ports", result.Detail)
	}
}

func TestPortsAllSatisfied(t *testing.T) {
	cond := NewPorts("localhost", 8000, 8001)
	cond.Dial = fakePortsDial(map[int]bool{8000: true, 8001: true})

	result := cond.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("status = %s, err = %v", result.Status, result.Err)
	}
	if result.Detail != "2 ports open" {
		t.Fatalf("detail = %q, want all open", result.Detail)
	}
}

func TestPortsAllUnsatisfied(t *testing.T) {
	cond := NewPorts("localhost", 8000, 8002)
	cond.Dial = fakePortsDial(map[int]bool{8000: true, 8002: true})

	result := cond.Check(t.Context())
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
	if result.Detail != "2 of 3 ports open" {
		t.Fatalf("detail = %q, want partial detail", result.Detail)
	}
}

func TestPortsInvalidConfigFatal(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*PortsCondition)
	}{
		{"empty host", func(c *PortsCondition) { c.Host = "" }},
		{"zero start", func(c *PortsCondition) { c.StartPort = 0 }},
		{"high end", func(c *PortsCondition) { c.EndPort = 65536 }},
		{"reversed", func(c *PortsCondition) { c.StartPort = 10; c.EndPort = 9 }},
		{"bad mode", func(c *PortsCondition) { c.Mode = "bad" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := NewPorts("localhost", 8000, 8001)
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

func TestPortsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cond := NewPorts("localhost", 8000, 8001)
	cond.Dial = fakePortsDial(map[int]bool{8000: true})
	result := cond.Check(ctx)
	if result.Status != CheckUnsatisfied {
		t.Fatalf("status = %s, want unsatisfied", result.Status)
	}
}

func TestPortsDescriptor(t *testing.T) {
	desc := NewPorts("localhost", 8000, 8002).Descriptor()
	if desc.Backend != "ports" || desc.Target != "localhost:8000-8002" {
		t.Fatalf("Descriptor() = %+v", desc)
	}
}

func fakePortsDial(open map[int]bool) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, address string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		_, portRaw, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portRaw)
		if err != nil {
			return nil, err
		}
		if open[port] {
			return noopConn{}, nil
		}
		return nil, fmt.Errorf("closed")
	}
}

type noopConn struct{}

func (noopConn) Read([]byte) (int, error)         { return 0, fmt.Errorf("not implemented") }
func (noopConn) Write([]byte) (int, error)        { return 0, fmt.Errorf("not implemented") }
func (noopConn) Close() error                     { return nil }
func (noopConn) LocalAddr() net.Addr              { return stringAddr("local") }
func (noopConn) RemoteAddr() net.Addr             { return stringAddr("remote") }
func (noopConn) SetDeadline(time.Time) error      { return nil }
func (noopConn) SetReadDeadline(time.Time) error  { return nil }
func (noopConn) SetWriteDeadline(time.Time) error { return nil }

type stringAddr string

func (a stringAddr) Network() string { return "test" }
func (a stringAddr) String() string  { return string(a) }

func TestPortsFakeDialRejectsBadAddress(t *testing.T) {
	_, err := fakePortsDial(nil)(t.Context(), "bad")
	if err == nil || !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("err = %v, want split host port error", err)
	}
}
