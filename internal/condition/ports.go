package condition

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type PortsMode string

const (
	PortsAll PortsMode = "all"
	PortsAny PortsMode = "any"
)

type PortsCondition struct {
	Host           string
	StartPort      int
	EndPort        int
	Mode           PortsMode
	AttemptTimeout time.Duration
	Dial           func(context.Context, string) (net.Conn, error)
}

func NewPorts(host string, startPort, endPort int) *PortsCondition {
	return &PortsCondition{
		Host:           host,
		StartPort:      startPort,
		EndPort:        endPort,
		Mode:           PortsAll,
		AttemptTimeout: 2 * time.Second,
	}
}

func (c *PortsCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "ports", Target: fmt.Sprintf("%s:%d-%d", c.Host, c.StartPort, c.EndPort)}
}

func (c *PortsCondition) Check(ctx context.Context) Result {
	if err := validatePortsConfig(c); err != nil {
		return Fatal(err)
	}
	open, closed, err := c.scan(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkPortsResult(open, closed, c)
}

func validatePortsConfig(c *PortsCondition) error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("ports host is required")
	}
	if !validPort(c.StartPort) || !validPort(c.EndPort) || c.StartPort > c.EndPort {
		return fmt.Errorf("invalid ports range %d-%d", c.StartPort, c.EndPort)
	}
	if c.Mode != PortsAll && c.Mode != PortsAny {
		return fmt.Errorf("invalid ports mode %q", c.Mode)
	}
	return nil
}

func validPort(port int) bool {
	return port >= 1 && port <= 65535
}

func (c *PortsCondition) scan(ctx context.Context) ([]int, []int, error) {
	open := []int{}
	closed := []int{}
	for port := c.StartPort; port <= c.EndPort; port++ {
		select {
		case <-ctx.Done():
			return open, closed, ctx.Err()
		default:
		}
		if c.portOpen(ctx, port) {
			open = append(open, port)
			if c.Mode == PortsAny {
				return open, closed, nil
			}
		} else {
			closed = append(closed, port)
		}
	}
	return open, closed, nil
}

func (c *PortsCondition) portOpen(ctx context.Context, port int) bool {
	conn, err := c.dial(ctx, net.JoinHostPort(c.Host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c *PortsCondition) dial(ctx context.Context, address string) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx, address)
	}
	timeout := c.AttemptTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	return dialer.DialContext(ctx, "tcp", address)
}

func checkPortsResult(open, closed []int, c *PortsCondition) Result {
	if c.Mode == PortsAny {
		if len(open) > 0 {
			return Satisfied(fmt.Sprintf("port %d open", open[0]))
		}
		detail := fmt.Sprintf("no open ports in %d-%d", c.StartPort, c.EndPort)
		return Unsatisfied(detail, errors.New(detail))
	}
	if len(closed) == 0 {
		return Satisfied(fmt.Sprintf("%d ports open", len(open)))
	}
	detail := fmt.Sprintf("%d of %d ports open", len(open), len(open)+len(closed))
	return Unsatisfied(detail, errors.New(detail))
}
