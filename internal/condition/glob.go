package condition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxGlobDirectoryEntries = 100000
const maxGlobMatches = 10000

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
	matches, err := c.glob(ctx)
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

func (c *GlobCondition) glob(ctx context.Context) ([]string, error) {
	if c.Glob != nil {
		return c.Glob(c.Pattern)
	}
	return boundedGlob(ctx, c.Pattern)
}

func boundedGlob(ctx context.Context, pattern string) ([]string, error) {
	dir, base := filepath.Split(pattern)
	if dir == "" {
		dir = "."
	}
	if hasGlobMeta(dir) {
		return nil, fmt.Errorf("glob directory wildcards are not supported")
	}
	if !hasGlobMeta(base) {
		if _, err := os.Lstat(pattern); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		return []string{pattern}, nil
	}
	return globDirectory(ctx, dir, base)
}

func globDirectory(ctx context.Context, dir, base string) ([]string, error) {
	handle, err := os.Open(dir) // #nosec G304 -- glob intentionally reads the user-selected directory.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	var matches []string
	scanned := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := handle.Readdirnames(256)
		scanned += len(names)
		if scanned > maxGlobDirectoryEntries {
			return nil, fmt.Errorf("glob scanned too many directory entries")
		}
		var matchErr error
		matches, matchErr = appendGlobMatches(matches, dir, base, names)
		if matchErr != nil {
			return nil, matchErr
		}
		if err == io.EOF {
			sort.Strings(matches)
			return matches, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func appendGlobMatches(matches []string, dir, base string, names []string) ([]string, error) {
	for _, name := range names {
		ok, err := filepath.Match(base, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		matches = append(matches, filepath.Join(dir, name))
		if len(matches) > maxGlobMatches {
			return nil, fmt.Errorf("glob matched too many paths")
		}
	}
	return matches, nil
}

func hasGlobMeta(value string) bool {
	return strings.ContainsAny(value, "*?[")
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
