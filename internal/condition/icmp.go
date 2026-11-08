package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const maxICMPTimeout = 24 * time.Hour

type ICMPCondition struct {
	Host           string
	Count          int
	AttemptTimeout time.Duration
	Ping           func(context.Context, string) error
}

func NewICMP(host string) *ICMPCondition {
	return &ICMPCondition{Host: host, Count: 1, AttemptTimeout: time.Second}
}

func (c *ICMPCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "icmp", Target: c.Host}
}

func (c *ICMPCondition) Check(ctx context.Context) Result {
	if err := validateICMPConfig(c); err != nil {
		return Fatal(err)
	}
	if err := c.ping(ctx); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("ping command not found"))
		}
		return Unsatisfied("", err)
	}
	return Satisfied("icmp ping succeeded")
}

func validateICMPConfig(c *ICMPCondition) error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("icmp host is required")
	}
	if err := rejectOptionLike("icmp host", c.Host); err != nil {
		return err
	}
	if c.Count <= 0 {
		return fmt.Errorf("--count must be positive")
	}
	if c.AttemptTimeout < 0 {
		return fmt.Errorf("--timeout must be non-negative")
	}
	if c.AttemptTimeout > maxICMPTimeout {
		return fmt.Errorf("--timeout must be at most %s", maxICMPTimeout)
	}
	return nil
}

func (c *ICMPCondition) ping(ctx context.Context) error {
	if c.Ping != nil {
		return c.Ping(ctx, c.Host)
	}
	return defaultPing(ctx, c.Host, c.Count, c.AttemptTimeout)
}

func defaultPing(ctx context.Context, host string, count int, timeout time.Duration) error {
	args := pingArgs(host, count, timeout)
	out, err := runLimitedCommand(ctx, "ping", args...)
	if err != nil {
		return classifyLimitedCommandError(err, out, ctx.Err())
	}
	return nil
}

func pingArgs(host string, count int, timeout time.Duration) []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutMillis(timeout)), host}
	case "darwin":
		return []string{"-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutMillis(timeout)), host}
	default:
		return []string{"-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSeconds(timeout)), host}
	}
}

func timeoutMillis(timeout time.Duration) int {
	if timeout <= 0 {
		return 1000
	}
	ms := int(timeout.Round(time.Millisecond) / time.Millisecond)
	if ms <= 0 {
		return 1
	}
	return ms
}

func timeoutSeconds(timeout time.Duration) int {
	if timeout <= 0 {
		return 1
	}
	seconds := int(timeout.Round(time.Second) / time.Second)
	if seconds <= 0 {
		return 1
	}
	return seconds
}
