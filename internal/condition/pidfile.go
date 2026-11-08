package condition

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const maxPIDFileBytes = 4096

type PIDFileCondition struct {
	Path      string
	State     ProcessState
	PIDExists func(context.Context, int) (bool, error)
}

func NewPIDFile(path string) *PIDFileCondition {
	return &PIDFileCondition{Path: path, State: ProcessRunning}
}

func (c *PIDFileCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "pidfile", Target: c.Path}
}

func (c *PIDFileCondition) Check(ctx context.Context) Result {
	if err := validatePIDFileConfig(c); err != nil {
		return Fatal(err)
	}
	pid, err := readPIDFile(ctx, c.Path)
	if err != nil {
		return checkPIDFileReadError(err, c.State)
	}
	exists, err := c.pidExists(ctx, pid)
	if err != nil {
		return Unsatisfied("", err)
	}
	return checkProcessFound(exists, c.State, fmt.Sprintf("pidfile pid %d", pid))
}

func validatePIDFileConfig(c *PIDFileCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("pidfile path is required")
	}
	switch c.State {
	case ProcessRunning, ProcessStopped:
		return nil
	default:
		return fmt.Errorf("unsupported pidfile state %q", c.State)
	}
}

func readPIDFile(ctx context.Context, path string) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	body, err := readPIDFileLimit(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile does not contain a positive pid")
	}
	return pid, nil
}

func readPIDFileLimit(path string) ([]byte, error) {
	file, _, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxPIDFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPIDFileBytes {
		return nil, fmt.Errorf("pidfile exceeds %d bytes", maxPIDFileBytes)
	}
	return body, nil
}

func checkPIDFileReadError(err error, want ProcessState) Result {
	if errorsIsNotExist(err) && want == ProcessStopped {
		return Satisfied("pidfile is absent")
	}
	if errorsIsNotExist(err) {
		return Unsatisfied("pidfile does not exist", err)
	}
	return Unsatisfied("", err)
}

func (c *PIDFileCondition) pidExists(ctx context.Context, pid int) (bool, error) {
	if c.PIDExists != nil {
		return c.PIDExists(ctx, pid)
	}
	return defaultPIDExists(ctx, pid)
}

func errorsIsNotExist(err error) bool {
	return os.IsNotExist(err)
}
