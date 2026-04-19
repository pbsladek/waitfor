package condition

import (
	"context"
	"net"
	"time"
)

type TCPCondition struct {
	Address        string
	AttemptTimeout time.Duration
}

func NewTCP(address string) *TCPCondition {
	return &TCPCondition{Address: address, AttemptTimeout: 2 * time.Second}
}

func (c *TCPCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "tcp", Target: c.Address}
}

func (c *TCPCondition) Check(ctx context.Context) Result {
	timeout := c.AttemptTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return Unsatisfied("", err)
	}
	_ = conn.Close()
	return Satisfied("connection established")
}
