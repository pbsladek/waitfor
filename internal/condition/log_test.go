package condition

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pbsladek/wait-for/internal/expr"
)

func TestLogMissingFile(t *testing.T) {
	c := NewLog(filepath.Join(t.TempDir(), "missing.log"))
	c.Contains = "ready"
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("missing file should not be satisfied")
	}
}

func TestLogContainsFromStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: ready\nother line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied; err = %v", result.Status, result.Err)
	}
}

func TestLogContainsNoMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: starting\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("non-matching content should not satisfy")
	}
}

func TestLogSkipsExistingContentByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("existing: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("existing content should be skipped without --from-start")
	}
}

func TestLogNewContentAfterInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old line\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"

	if first := c.Check(t.Context()); first.Status == CheckSatisfied {
		t.Fatal("first poll should not be satisfied (existing content skipped)")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- test appends to path created by t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("service: ready\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	if second := c.Check(t.Context()); second.Status != CheckSatisfied {
		t.Fatalf("second poll Status = %s, want satisfied", second.Status)
	}
}

func TestLogRegexMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("2024-01-01 ERROR: connection timeout\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Regex = regexp.MustCompile(`ERROR:.*timeout`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied", result.Status)
	}
}

func TestLogRegexNoMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("INFO: all good\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Regex = regexp.MustCompile(`ERROR`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("non-matching regex should not satisfy")
	}
}

func TestLogJSONExprMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	lines := `{"level":"info","msg":"starting"}` + "\n" + `{"level":"ready","msg":"up"}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.JSONExpr = expr.MustCompile(`.level == "ready"`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied", result.Status)
	}
}

func TestLogJSONExprSkipsNonJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("plain text line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.JSONExpr = expr.MustCompile(`.level == "ready"`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("non-JSON line should not match JSON expression")
	}
}

func TestLogAndMatchersAllMustPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: ready INFO\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.Regex = regexp.MustCompile(`ERROR`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("both matchers must pass; regex should fail here")
	}
}

// ── matched line in detail ────────────────────────────────────────────────────

func TestLogMatchedLineAppearsInDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: ready at port 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied", result.Status)
	}
	if !strings.Contains(result.Detail, "ready at port 8080") {
		t.Fatalf("Detail = %q, want matched line content", result.Detail)
	}
}

func TestLogDetailTruncatesLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	long := strings.Repeat("x", maxMatchDetail+50) + "\n"
	if err := os.WriteFile(path, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Regex = regexp.MustCompile(`x+`)
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied", result.Status)
	}
	if !strings.HasSuffix(result.Detail, "...") {
		t.Fatalf("Detail = %q, want truncated with ...", result.Detail)
	}
}

func TestLogMatchesLineLongerThanScannerDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	long := strings.Repeat("x", 70*1024) + " ready\n"
	if err := os.WriteFile(path, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied for long line; err = %v", result.Status, result.Err)
	}
}

func TestLogReportsScannerError(t *testing.T) {
	c := NewLog("unused")
	result, complete := c.scanLines(t.Context(), []byte(strings.Repeat("x", int(maxLogScanBytes)+1)))
	if result.Status == CheckSatisfied {
		t.Fatal("oversized log token should not be satisfied")
	}
	if complete {
		t.Fatal("oversized log token should not be marked as completely scanned")
	}
	if result.Err == nil {
		t.Fatal("oversized log token should report scanner error")
	}
	if !strings.Contains(result.Detail, "log scan failed") {
		t.Fatalf("Detail = %q, want log scan failure", result.Detail)
	}
}

func TestLogDoesNotAdvanceOffsetWhenScanCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("service: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := c.readNewContent(info)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, complete := c.scanLines(ctx, chunk.data)
	if complete {
		t.Fatal("cancelled scan should not be complete")
	}
	if result.Err == nil {
		t.Fatal("cancelled scan should return context error")
	}
	if c.offset != 0 {
		t.Fatalf("offset = %d, want 0 after cancelled scan", c.offset)
	}
	if retry := c.Check(t.Context()); retry.Status != CheckSatisfied {
		t.Fatalf("retry Status = %s, want satisfied after rereading uncommitted bytes", retry.Status)
	}
}

func TestLogDoesNotCommitMatchesOnScannerError(t *testing.T) {
	c := NewLog("unused")
	c.Contains = "ok"
	c.MinMatches = 2
	data := []byte("ok\n" + strings.Repeat("x", int(maxLogScanBytes)+1))

	result, complete := c.scanLines(t.Context(), data)
	if complete {
		t.Fatal("scanner error should not be complete")
	}
	if result.Err == nil {
		t.Fatal("scanner error should be returned")
	}
	if c.matchCount != 0 {
		t.Fatalf("matchCount = %d, want 0 after incomplete scan", c.matchCount)
	}
}

func TestLogScansFileCreatedAfterWaitStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	c := NewLog(path)
	c.Contains = "ready"

	first := c.Check(t.Context())
	if first.Status == CheckSatisfied {
		t.Fatal("missing log file should not satisfy")
	}
	if err := os.WriteFile(path, []byte("service ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied for file created after wait began; err = %v detail = %q", result.Status, result.Err, result.Detail)
	}
}

func TestLogDetectsCopytruncateRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old line\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("initial existing content should not satisfy by default")
	}
	if err := os.WriteFile(path, []byte("ready after truncate\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied after copytruncate; err = %v detail = %q", result.Status, result.Err, result.Detail)
	}
}

// ── --tail ───────────────────────────────────────────────────────────────────

func TestLogTailScansLastNLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	content := "line one\nline two\nline three: ready\nline four\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// --tail 2 covers "line three: ready" and "line four" only.
	c := NewLog(path)
	c.Contains = "ready"
	c.Tail = 2
	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied; tail=2 should include 'line three'", result.Status)
	}
}

func TestLogTailExcludesOlderLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	content := "line one: ready\nline two\nline three\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// --tail 2 covers only "line two" and "line three"; "ready" is in line one.
	c := NewLog(path)
	c.Contains = "ready"
	c.Tail = 2
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("tail=2 should not include line one where 'ready' appears")
	}
}

func TestLogTailFewerLinesThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("only: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --tail 10 on a 1-line file should read from the beginning.
	c := NewLog(path)
	c.Contains = "ready"
	c.Tail = 10
	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied when tail exceeds file length", result.Status)
	}
}

// ── --min-matches ─────────────────────────────────────────────────────────────

func TestLogMinMatchesAccumulatesAcrossPolls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("heartbeat\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "heartbeat"
	c.MinMatches = 3
	c.FromStart = true

	// First poll finds 1 match — not enough.
	if r := c.Check(t.Context()); r.Status == CheckSatisfied {
		t.Fatal("1 match should not satisfy min-matches=3")
	}

	// Append two more matching lines.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- test appends to path created by t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("heartbeat\nheartbeat\n")
	_ = f.Close()

	// Second poll finds 2 more — total 3, now satisfied.
	if r := c.Check(t.Context()); r.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied after 3 cumulative matches", r.Status)
	}
}

func TestLogMinMatchesDetailShowsCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	lines := "ok\nok\nok\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ok"
	c.MinMatches = 3
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied", result.Status)
	}
	if !strings.Contains(result.Detail, "3 matches") {
		t.Fatalf("Detail = %q, want match count", result.Detail)
	}
}

func TestLogMinMatchesUnsatisfiedDetailShowsProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ok"
	c.MinMatches = 3
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status == CheckSatisfied {
		t.Fatal("1 match should not satisfy min-matches=3")
	}
	if !strings.Contains(result.Detail, "1 of 3") {
		t.Fatalf("Detail = %q, want progress fraction", result.Detail)
	}
}

// ── --exclude ────────────────────────────────────────────────────────────────

func TestLogExcludeFiltersLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// "ready" appears on a DEBUG line (should be excluded) and an INFO line (should match).
	content := "DEBUG ready check\nINFO service ready\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// With exclude, the DEBUG line is dropped; INFO line matches.
	c := NewLog(path)
	c.Contains = "ready"
	c.Exclude = regexp.MustCompile(`^DEBUG`)
	c.MinMatches = 1
	c.FromStart = true
	result := c.Check(t.Context())
	if result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied; INFO line should match after DEBUG excluded", result.Status)
	}
}

func TestLogExcludeBlocksAllMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("DEBUG ready\nDEBUG ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ready"
	c.Exclude = regexp.MustCompile(`^DEBUG`)
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("all lines excluded — should not be satisfied")
	}
}

func TestLogExcludeAndMinMatchesCountOnlyNonExcluded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// 3 DEBUG lines (excluded) + 2 INFO lines (counted). MinMatches=2 should satisfy.
	content := "DEBUG ok\nINFO ok\nDEBUG ok\nINFO ok\nDEBUG ok\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewLog(path)
	c.Contains = "ok"
	c.Exclude = regexp.MustCompile(`^DEBUG`)
	c.MinMatches = 2
	c.FromStart = true
	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s, want satisfied with 2 non-excluded matches", result.Status)
	}
}

// ── rotation ─────────────────────────────────────────────────────────────────

func TestLogRotationResetsOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewLog(path)
	c.Contains = "ready"
	c.FromStart = true
	_ = c.Check(t.Context()) // advances offset past "old line\n"

	// Simulate rotation: replace file with a new inode.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("service: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result := c.Check(t.Context()); result.Status != CheckSatisfied {
		t.Fatalf("Status = %s after rotation, want satisfied", result.Status)
	}
}

func TestLogRotationResetsMatchCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	// Get 2 matches with min-matches=3, then rotate.
	if err := os.WriteFile(path, []byte("ok\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewLog(path)
	c.Contains = "ok"
	c.MinMatches = 3
	c.FromStart = true
	_ = c.Check(t.Context()) // matchCount = 2

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// New file: only 1 match — should NOT satisfy since rotation resets matchCount.
	if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result := c.Check(t.Context()); result.Status == CheckSatisfied {
		t.Fatal("matchCount should reset on rotation; 1 match should not satisfy min-matches=3")
	}
}

// ── descriptor ───────────────────────────────────────────────────────────────

func TestLogDescriptor(t *testing.T) {
	c := NewLog("/var/log/app.log")
	d := c.Descriptor()
	if d.Backend != "log" {
		t.Fatalf("Backend = %q, want log", d.Backend)
	}
	if d.Target != "/var/log/app.log" {
		t.Fatalf("Target = %q, want /var/log/app.log", d.Target)
	}
}
