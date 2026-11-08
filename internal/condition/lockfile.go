package condition

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

type LockfileState string

const (
	LockfilePresent LockfileState = "present"
	LockfileAbsent  LockfileState = "absent"
)

type LockfileCondition struct {
	Path      string
	State     LockfileState
	OlderThan time.Duration
}

func NewLockfile(path string) *LockfileCondition {
	return &LockfileCondition{Path: path, State: LockfileAbsent}
}

func (c *LockfileCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "lockfile", Target: c.Path}
}

func (c *LockfileCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateLockfileConfig(c); err != nil {
		return Fatal(err)
	}
	info, err := os.Lstat(c.Path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return Unsatisfied("", err)
	}
	return checkLockfileExists(exists, info, c)
}

func validateLockfileConfig(c *LockfileCondition) error {
	if strings.TrimSpace(c.Path) == "" {
		return fmt.Errorf("lockfile path is required")
	}
	switch c.State {
	case LockfilePresent, LockfileAbsent:
	default:
		return fmt.Errorf("unsupported lockfile state %q", c.State)
	}
	if c.OlderThan < 0 {
		return fmt.Errorf("--older-than must be non-negative")
	}
	if c.OlderThan > 0 && c.State != LockfilePresent {
		return fmt.Errorf("--older-than requires --present")
	}
	return nil
}

func checkLockfileExists(exists bool, info os.FileInfo, c *LockfileCondition) Result {
	if exists && c.State == LockfilePresent {
		return checkLockfileAge(info, c.OlderThan)
	}
	if !exists && c.State == LockfileAbsent {
		return Satisfied("lockfile is absent")
	}
	if c.State == LockfilePresent {
		return Unsatisfied("lockfile does not exist", fmt.Errorf("lockfile does not exist"))
	}
	return Unsatisfied("lockfile still exists", fmt.Errorf("lockfile still exists"))
}

func checkLockfileAge(info os.FileInfo, olderThan time.Duration) Result {
	if olderThan == 0 {
		return Satisfied("lockfile exists")
	}
	age := time.Since(info.ModTime())
	if age >= olderThan {
		return Satisfied(fmt.Sprintf("lockfile age %s is at least %s", age.Round(time.Millisecond), olderThan))
	}
	return Unsatisfied("lockfile is not old enough", fmt.Errorf("lockfile age %s is less than %s", age.Round(time.Millisecond), olderThan))
}
