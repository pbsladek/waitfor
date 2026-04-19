package condition

import (
	"net"
	"testing"
)

func TestTCPConditionSatisfied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	result := NewTCP(listener.Addr().String()).Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Satisfied = false, err = %v", result.Err)
	}
	<-done
}

func TestTCPConditionRefused(t *testing.T) {
	// Grab an address then immediately close the listener so connections are refused.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	result := NewTCP(addr).Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("expected unsatisfied for refused connection")
	}
}

func TestTCPConditionDescriptor(t *testing.T) {
	c := NewTCP("localhost:5432")
	d := c.Descriptor()
	if d.Backend != "tcp" {
		t.Fatalf("Backend = %q, want tcp", d.Backend)
	}
	if d.Target != "localhost:5432" {
		t.Fatalf("Target = %q, want localhost:5432", d.Target)
	}
}
