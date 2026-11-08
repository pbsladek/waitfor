package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type LaunchdState string

const (
	LaunchdLoaded  LaunchdState = "loaded"
	LaunchdRunning LaunchdState = "running"
)

type LaunchdCondition struct {
	Label string
	State LaunchdState
	Print func(context.Context, string) (string, error)
}

func NewLaunchd(label string) *LaunchdCondition {
	return &LaunchdCondition{Label: label, State: LaunchdRunning}
}

func (c *LaunchdCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "launchd", Target: c.Label}
}

func (c *LaunchdCondition) Check(ctx context.Context) Result {
	if err := validateLaunchdConfig(c); err != nil {
		return Fatal(err)
	}
	output, err := c.print(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("launchctl command not found"))
		}
		return Unsatisfied("", err)
	}
	if c.State == LaunchdLoaded {
		return Satisfied("launchd service is loaded")
	}
	return checkLaunchdRunning(output)
}

func validateLaunchdConfig(c *LaunchdCondition) error {
	if strings.TrimSpace(c.Label) == "" {
		return fmt.Errorf("launchd label is required")
	}
	if err := rejectOptionLike("launchd label", c.Label); err != nil {
		return err
	}
	switch c.State {
	case LaunchdLoaded, LaunchdRunning:
		return nil
	default:
		return fmt.Errorf("unsupported launchd state %q", c.State)
	}
}

func checkLaunchdRunning(output string) Result {
	if launchdOutputHasPID(output) {
		return Satisfied("launchd service is running")
	}
	return Unsatisfied("launchd service is loaded but not running", fmt.Errorf("launchd service is not running"))
}

func launchdOutputHasPID(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" && fields[2] != "0" {
			return true
		}
	}
	return false
}

func (c *LaunchdCondition) print(ctx context.Context) (string, error) {
	if c.Print != nil {
		return c.Print(ctx, c.Label)
	}
	return defaultLaunchctlPrint(ctx, c.Label)
}

func defaultLaunchctlPrint(ctx context.Context, label string) (string, error) {
	out, err := runLimitedCommand(ctx, "launchctl", "print", label)
	if err != nil {
		return "", classifyLimitedCommandError(err, out, ctx.Err())
	}
	return string(out.stdout), nil
}
