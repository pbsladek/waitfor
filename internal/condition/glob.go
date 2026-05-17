package condition

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type GlobCondition struct {
	Pattern  string
	MinCount int
	MaxCount int
	Absent   bool
	Glob     func(string) ([]string, error)
}

func NewGlob(pattern string) *GlobCondition {
	return &GlobCondition{Pattern: pattern, MinCount: 1, MaxCount: -1}
}

func (c *GlobCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "glob", Target: c.Pattern}
}

func (c *GlobCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}
	if err := validateGlobConfig(c); err != nil {
		return Fatal(err)
	}
	matches, err := c.glob()
	if err != nil {
		return Fatal(err)
	}
	return checkGlobCount(len(matches), c)
}

func validateGlobConfig(c *GlobCondition) error {
	if strings.TrimSpace(c.Pattern) == "" {
		return fmt.Errorf("glob pattern is required")
	}
	if c.MinCount < 0 {
		return fmt.Errorf("glob min-count cannot be negative")
	}
	if c.MaxCount < -1 {
		return fmt.Errorf("glob max-count cannot be less than -1")
	}
	if c.MaxCount >= 0 && c.MinCount > c.MaxCount {
		return fmt.Errorf("glob min-count cannot exceed max-count")
	}
	if c.Absent && c.MinCount > 0 {
		return fmt.Errorf("glob absent cannot require a positive min-count")
	}
	return nil
}

func (c *GlobCondition) glob() ([]string, error) {
	if c.Glob != nil {
		return c.Glob(c.Pattern)
	}
	return filepath.Glob(c.Pattern)
}

func checkGlobCount(count int, c *GlobCondition) Result {
	if c.Absent {
		return checkGlobAbsent(count)
	}
	if count < c.MinCount {
		detail := fmt.Sprintf("%d matches, need at least %d", count, c.MinCount)
		return Unsatisfied(detail, errors.New(detail))
	}
	if c.MaxCount >= 0 && count > c.MaxCount {
		detail := fmt.Sprintf("%d matches, need at most %d", count, c.MaxCount)
		return Unsatisfied(detail, errors.New(detail))
	}
	return Satisfied(globSatisfiedDetail(count))
}

func checkGlobAbsent(count int) Result {
	if count == 0 {
		return Satisfied("no matches")
	}
	detail := fmt.Sprintf("%d matches still present", count)
	return Unsatisfied(detail, errors.New(detail))
}

func globSatisfiedDetail(count int) string {
	if count == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", count)
}
