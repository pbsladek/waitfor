package condition

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

type UnixCondition struct {
	Path           string
	AttemptTimeout time.Duration
	Dial           func(context.Context, string) (net.Conn, error)
}

func NewUnix(path string) *UnixCondition {
	return &UnixCondition{Path: path, AttemptTimeout: 2 * time.Second}
}

func (c *UnixCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "unix", Target: c.Path}
}

func (c *UnixCondition) Check(ctx context.Context) Result {
	if err := validateUnixConfig(c); err != nil {
		return Fatal(err)
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	_ = conn.Close()
	return Satisfied("connection established")
}

func validateUnixConfig(c *UnixCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("unix socket path is required")
	}
	return nil
}

func (c *UnixCondition) dial(ctx context.Context) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, c.Path)
	}
	timeout := c.AttemptTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	return dialer.DialContext(ctx, "unix", c.Path)
}
