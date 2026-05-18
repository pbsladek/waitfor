package condition

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessState string

const (
	ProcessRunning ProcessState = "running"
	ProcessStopped ProcessState = "stopped"
)

type ProcessCondition struct {
	PID       int
	Name      string
	State     ProcessState
	PIDExists func(context.Context, int) (bool, error)
	List      func(context.Context) ([]ProcessInfo, error)
}

type ProcessInfo struct {
	PID     int
	Name    string
	Command string
}

func NewProcess() *ProcessCondition {
	return &ProcessCondition{State: ProcessRunning}
}

func (c *ProcessCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "process", Target: processTarget(c)}
}

func (c *ProcessCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateProcessConfig(c); err != nil {
		return Fatal(err)
	}
	if c.PID > 0 {
		return c.checkPID(ctx)
	}
	return c.checkName(ctx)
}

func (c *ProcessCondition) checkPID(ctx context.Context) Result {
	exists, err := c.pidExists(ctx)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkProcessFound(exists, c.State, fmt.Sprintf("pid %d", c.PID))
}

func (c *ProcessCondition) checkName(ctx context.Context) Result {
	processes, err := c.list(ctx)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Fatal(fmt.Errorf("ps command not found"))
		}
		return Unsatisfied("", err)
	}
	count := countNamedProcesses(processes, c.Name)
	return checkProcessFound(count > 0, c.State, processNameDetail(c.Name, count))
}

func (c *ProcessCondition) pidExists(ctx context.Context) (bool, error) {
	if c.PIDExists != nil {
		return c.PIDExists(ctx, c.PID)
	}
	return defaultPIDExists(ctx, c.PID)
}

func (c *ProcessCondition) list(ctx context.Context) ([]ProcessInfo, error) {
	if c.List != nil {
		return c.List(ctx)
	}
	return defaultProcessList(ctx)
}

func validateProcessConfig(c *ProcessCondition) error {
	if c.PID <= 0 && strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("process requires exactly one of --pid or --name")
	}
	if c.PID > 0 && strings.TrimSpace(c.Name) != "" {
		return fmt.Errorf("--pid and --name are mutually exclusive")
	}
	switch c.State {
	case ProcessRunning, ProcessStopped:
		return nil
	default:
		return fmt.Errorf("unsupported process state %q", c.State)
	}
}

func checkProcessFound(found bool, want ProcessState, detail string) Result {
	if found && want == ProcessRunning {
		return Satisfied(detail + " is running")
	}
	if !found && want == ProcessStopped {
		return Satisfied(detail + " is stopped")
	}
	if want == ProcessRunning {
		return Unsatisfied(detail+" is not running", fmt.Errorf("process is not running"))
	}
	return Unsatisfied(detail+" is still running", fmt.Errorf("process is still running"))
}

func countNamedProcesses(processes []ProcessInfo, name string) int {
	want := filepath.Base(strings.TrimSpace(name))
	count := 0
	for _, process := range processes {
		if processNameMatches(process, want) {
			count++
		}
	}
	return count
}

func processNameMatches(process ProcessInfo, want string) bool {
	if filepath.Base(firstCommandToken(process.Name)) == want {
		return true
	}
	return filepath.Base(firstCommandToken(process.Command)) == want
}

func firstCommandToken(command string) string {
	command = strings.TrimSpace(command)
	for i := 0; i < len(command); i++ {
		if isProcessTokenSpace(command[i]) {
			return command[:i]
		}
	}
	return command
}

func isProcessTokenSpace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func processTarget(c *ProcessCondition) string {
	if c.PID > 0 {
		return fmt.Sprintf("pid %d", c.PID)
	}
	return strings.TrimSpace(c.Name)
}

func processNameDetail(name string, count int) string {
	detail := fmt.Sprintf("name %q", strings.TrimSpace(name))
	if count > 0 {
		detail += fmt.Sprintf(" matched %d process(es)", count)
	}
	return detail
}

func defaultProcessList(ctx context.Context) ([]ProcessInfo, error) {
	out, err := runLimitedCommand(ctx, "ps", "-axo", "pid=,comm=,args=")
	if err != nil {
		return nil, classifyProcessListError(err, ctx.Err())
	}
	if out.truncated {
		return nil, fmt.Errorf("process list output exceeded %d bytes", maxExternalCommandOutputBytes)
	}
	return parseProcessTable(string(out.stdout)), nil
}

func classifyProcessListError(err, ctxErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, exec.ErrNotFound) {
		return exec.ErrNotFound
	}
	return fmt.Errorf("list processes: %w", err)
}

func parseProcessTable(output string) []ProcessInfo {
	lines := strings.Split(output, "\n")
	processes := make([]ProcessInfo, 0, len(lines))
	for _, line := range lines {
		if process, ok := parseProcessLine(line); ok {
			processes = append(processes, process)
		}
	}
	return processes
}

func parseProcessLine(line string) (ProcessInfo, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ProcessInfo{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return ProcessInfo{}, false
	}
	process := ProcessInfo{PID: pid, Name: fields[1]}
	if len(fields) > 2 {
		process.Command = strings.Join(fields[2:], " ")
	}
	return process, true
}
