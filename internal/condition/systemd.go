package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type SystemdState string

const (
	SystemdActive   SystemdState = "active"
	SystemdInactive SystemdState = "inactive"
	SystemdFailed   SystemdState = "failed"
)

type SystemdCondition struct {
	Unit  string
	State SystemdState
	Show  func(context.Context, string) (SystemdUnitState, error)
}

type SystemdUnitState struct {
	LoadState   string
	ActiveState string
	SubState    string
}

func NewSystemd(unit string) *SystemdCondition {
	return &SystemdCondition{Unit: unit, State: SystemdActive}
}

func (c *SystemdCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "systemd", Target: c.Unit}
}

func (c *SystemdCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateSystemdConfig(c); err != nil {
		return Fatal(err)
	}
	state, err := c.show(ctx)
	if err != nil {
		if fatalSystemdError(err) {
			return Fatal(err)
		}
		return Unsatisfied("", err)
	}
	return checkSystemdState(state, c.State)
}

func (c *SystemdCondition) show(ctx context.Context) (SystemdUnitState, error) {
	if c.Show != nil {
		return c.Show(ctx, c.Unit)
	}
	return defaultSystemdShow(ctx, c.Unit)
}

func validateSystemdConfig(c *SystemdCondition) error {
	if strings.TrimSpace(c.Unit) == "" {
		return fmt.Errorf("systemd unit is required")
	}
	if err := rejectOptionLike("systemd unit", c.Unit); err != nil {
		return err
	}
	switch c.State {
	case SystemdActive, SystemdInactive, SystemdFailed:
		return nil
	default:
		return fmt.Errorf("unsupported systemd state %q", c.State)
	}
}

func checkSystemdState(state SystemdUnitState, want SystemdState) Result {
	if strings.EqualFold(state.LoadState, "not-found") {
		return Unsatisfied("unit not found", fmt.Errorf("systemd unit not found"))
	}
	active := strings.ToLower(state.ActiveState)
	if active == string(want) {
		return Satisfied(systemdDetail(state))
	}
	detail := fmt.Sprintf("active state %s, expected %s", state.ActiveState, want)
	return Unsatisfied(detail, errors.New(detail))
}

func defaultSystemdShow(ctx context.Context, unit string) (SystemdUnitState, error) {
	out, err := runLimitedCommand(ctx, "systemctl", "show", unit, "--property=LoadState,ActiveState,SubState")
	if err != nil {
		return SystemdUnitState{}, classifySystemdCommandError(err, string(out.combined(maxExternalCommandOutputBytes)), ctx.Err())
	}
	return parseSystemdShow(string(out.stdout)), nil
}

func classifySystemdCommandError(err error, output string, ctxErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, exec.ErrNotFound) {
		return exec.ErrNotFound
	}
	detail := strings.TrimSpace(output)
	if detail == "" {
		return fmt.Errorf("systemctl show failed: %w", err)
	}
	if systemdUnavailableDetail(detail) {
		return fmt.Errorf("systemd unavailable: %s", detail)
	}
	return fmt.Errorf("systemctl show failed: %s", detail)
}

func fatalSystemdError(err error) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "systemd unavailable")
}

func systemdUnavailableDetail(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "system has not been booted") ||
		strings.Contains(lower, "failed to connect to bus") ||
		strings.Contains(lower, "host is down")
}

func parseSystemdShow(output string) SystemdUnitState {
	var state SystemdUnitState
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		assignSystemdProperty(&state, key, value)
	}
	return state
}

func assignSystemdProperty(state *SystemdUnitState, key, value string) {
	switch key {
	case "LoadState":
		state.LoadState = value
	case "ActiveState":
		state.ActiveState = value
	case "SubState":
		state.SubState = value
	}
}

func systemdDetail(state SystemdUnitState) string {
	details := []string{"active state " + state.ActiveState}
	if state.LoadState != "" {
		details = append(details, "load state "+state.LoadState)
	}
	if state.SubState != "" {
		details = append(details, "substate "+state.SubState)
	}
	return strings.Join(details, ", ")
}
