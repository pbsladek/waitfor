package condition

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/pbsladek/wait-for/internal/expr"
)

const (
	maxLogScanBytes  int64 = 10 * 1024 * 1024
	maxTailScanBytes int64 = 1 * 1024 * 1024
	maxMatchDetail         = 200
)

// LogCondition tails a file and returns Satisfied when enough matching lines
// appear. It tracks a byte offset so each poll reads only new content.
// File rotation is detected via os.SameFile and resets all state.
type LogCondition struct {
	Path       string
	Contains   string
	Regex      *regexp.Regexp
	Exclude    *regexp.Regexp // lines matching Exclude are skipped before other checks
	JSONExpr   *expr.Expression
	FromStart  bool
	Tail       int // scan last N lines of existing content before tailing
	MinMatches int // require N cumulative matching lines (0 or 1 both mean 1)

	// mutable per-instance state; safe because the runner calls Check
	// from exactly one goroutine per condition.
	offset      int64
	prevInfo    os.FileInfo
	initialized bool
	matchCount  int
}

func NewLog(path string) *LogCondition {
	return &LogCondition{Path: path}
}

func (c *LogCondition) Descriptor() Descriptor {
	return Descriptor{Backend: "log", Target: c.Path, Name: fmt.Sprintf("log %s", c.Path)}
}

func (c *LogCondition) Check(ctx context.Context) Result {
	select {
	case <-ctx.Done():
		return Unsatisfied("", ctx.Err())
	default:
	}

	info, err := os.Stat(c.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Unsatisfied("file does not exist", err)
		}
		return Unsatisfied("", err)
	}

	data, err := c.readNewContent(info)
	if err != nil {
		return Unsatisfied("", err)
	}

	return c.scanLines(ctx, data)
}

func (c *LogCondition) readNewContent(info os.FileInfo) ([]byte, error) {
	c.resetOnRotation(info)
	if !c.initialized {
		if err := c.initOffset(info); err != nil {
			return nil, err
		}
		c.initialized = true
	}
	c.prevInfo = info

	f, err := os.Open(c.Path) // #nosec G304 -- log polling intentionally reads the user-selected target.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLogScanBytes))
	if err != nil {
		return nil, err
	}
	c.offset += int64(len(data))
	return data, nil
}

func (c *LogCondition) resetOnRotation(info os.FileInfo) {
	if c.prevInfo != nil && !os.SameFile(c.prevInfo, info) {
		c.offset = 0
		c.initialized = false
		c.matchCount = 0
	}
}

func (c *LogCondition) initOffset(info os.FileInfo) error {
	if c.FromStart {
		c.offset = 0
		return nil
	}
	if c.Tail > 0 {
		off, err := computeTailOffset(c.Path, info.Size(), c.Tail)
		if err != nil {
			return err
		}
		c.offset = off
		return nil
	}
	c.offset = info.Size()
	return nil
}

func (c *LogCondition) scanLines(ctx context.Context, data []byte) Result {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return Unsatisfied("", ctx.Err())
		default:
		}
		line := scanner.Bytes()
		if c.matchLine(line) {
			c.matchCount++
			if c.matchCount >= c.requiredMatches() {
				return Satisfied(c.satisfiedDetail(line))
			}
		}
	}
	return c.unsatisfiedResult()
}

func (c *LogCondition) requiredMatches() int {
	if c.MinMatches > 0 {
		return c.MinMatches
	}
	return 1
}

func (c *LogCondition) satisfiedDetail(line []byte) string {
	detail := truncateLogLine(line)
	if c.requiredMatches() > 1 {
		return fmt.Sprintf("%d matches; last: %s", c.matchCount, detail)
	}
	return "matched: " + detail
}

func (c *LogCondition) unsatisfiedResult() Result {
	if c.matchCount > 0 {
		return Unsatisfied(
			fmt.Sprintf("%d of %d required matches", c.matchCount, c.requiredMatches()),
			fmt.Errorf("not enough matching lines yet"),
		)
	}
	return Unsatisfied("no matching log line", fmt.Errorf("no matching log line found"))
}

// matchLine returns true when line passes all configured matchers (AND semantics).
// Exclude is applied first as a pre-filter; if it matches, the line is dropped.
func (c *LogCondition) matchLine(line []byte) bool {
	if c.Exclude != nil && c.Exclude.Match(line) {
		return false
	}
	if c.Contains != "" && !bytes.Contains(line, []byte(c.Contains)) {
		return false
	}
	if c.Regex != nil && !c.Regex.Match(line) {
		return false
	}
	if c.JSONExpr != nil {
		ok, _, _ := c.JSONExpr.EvaluateJSON(line)
		return ok
	}
	return true
}

func truncateLogLine(line []byte) string {
	if len(line) > maxMatchDetail {
		return string(line[:maxMatchDetail]) + "..."
	}
	return string(line)
}

// computeTailOffset returns the file byte offset at which the last `lines`
// lines begin, reading at most maxTailScanBytes from the end of the file.
func computeTailOffset(path string, size int64, lines int) (int64, error) {
	if lines <= 0 || size == 0 {
		return size, nil
	}
	readFrom := size - maxTailScanBytes
	if readFrom < 0 {
		readFrom = 0
	}
	f, err := os.Open(path) // #nosec G304 -- log polling intentionally reads the user-selected target.
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(readFrom, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTailScanBytes))
	if err != nil {
		return 0, err
	}
	return findTailStart(data, readFrom, lines), nil
}

// findTailStart scans data backward to find the offset of the line that is
// `lines` lines from the end. base is the file offset of data[0].
func findTailStart(data []byte, base int64, lines int) int64 {
	pos := len(data)
	if pos > 0 && data[pos-1] == '\n' {
		pos-- // a trailing newline is not a line of its own
	}
	count := 0
	for pos > 0 {
		pos--
		if data[pos] == '\n' {
			count++
			if count == lines {
				return base + int64(pos) + 1
			}
		}
	}
	return base
}
