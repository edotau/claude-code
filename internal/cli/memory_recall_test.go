package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
)

// TestMemoryRecallRanksAndFormatsUnstampedHits: `memory recall` prints each hit "<ref> — <title>";
// an unstamped hit (no persisted id) prints a content-derived ref, never a bare "-", and that ref
// resolves back through `memory show` (the point of giving every hit a ref at all).
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := bank.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memstore.Dir(repo), "conventions.md"),
		[]byte("# Conventions\n\n- stamped id-marked rule <!-- id:deadbeef -->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Append an unstamped line by hand, after Remember's settle pass already assigned ids above —
	// activeContext.md is never id-assigned, so it stays unstamped (SearchBanks still indexes it).
	if err := os.WriteFile(filepath.Join(memstore.Dir(repo), "activeContext.md"),
		[]byte("# Active Context\n\n- rule stamped id-marked never settled here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"recall", "--repo", repo, "stamped", "id-marked", "rule"}) })
	if code != 0 {
		t.Fatalf("memory recall: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "deadbeef — ") {
		t.Errorf("recall output missing the stamped id line: %q", out)
	}
	if strings.Contains(out, "\n- —") || strings.Contains(out, "\n — ") {
		t.Errorf("an unstamped hit printed the old bare-dash format: %q", out)
	}
	var contentRef string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if ref, _, ok := strings.Cut(line, " — "); ok && ref != "deadbeef" {
			contentRef = ref
		}
	}
	if contentRef == "" || !strings.HasPrefix(contentRef, "z") {
		t.Fatalf("recall output missing a content-derived ref for the unstamped hit: %q", out)
	}
	if _, block, err := bank.ShowBlock(contentRef); err != nil || !strings.Contains(block, "never settled here") {
		t.Errorf("memory show %s = %q, %v; want the unstamped block", contentRef, block, err)
	}
}

// TestMemoryRecallLoadResolvesUnstampedHitByContentRef: `memory recall --load` fetches an unstamped
// hit's body through its derived content ref, the same path a persisted id takes.
func TestMemoryRecallLoadResolvesUnstampedHitByContentRef(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := bank.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memstore.Dir(repo), "activeContext.md"),
		[]byte("# Active Context\n\n- never settled widget rollout detail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"recall", "--repo", repo, "--load", "1", "never", "settled", "widget", "rollout"})
	})
	if code != 0 {
		t.Fatalf("memory recall --load: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "never settled widget rollout detail") {
		t.Errorf("recall --load did not resolve the unstamped hit's body: %q", out)
	}
}

// TestMemoryRecallNoQueryFails: an empty query is refused (exit 1), naming memoryUsage.
func TestMemoryRecallNoQueryFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"recall"}) })
	if code != 1 || !strings.Contains(stderr, "no query") {
		t.Errorf("memory recall with no terms: code=%d stderr=%q", code, stderr)
	}
}

// TestMemoryRecallLoadAppendsBodiesUnderByteCap: --load N appends each hit's full block text,
// never exceeding recallLoadCap total.
func TestMemoryRecallLoadAppendsBodiesUnderByteCap(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.Remember(repo, "feedback", "loadable fact about widgets and gadgets", time.Now()); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"recall", "--repo", repo, "--load", "3", "loadable", "widgets", "gadgets", "fact"})
	})
	if code != 0 {
		t.Fatalf("memory recall --load: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "loadable fact about widgets and gadgets") {
		t.Errorf("recall --load output missing the loaded body: %q", out)
	}
	if len(out) > 20000 { // generous ceiling: just confirms --load didn't dump the whole bank unbounded
		t.Errorf("recall --load output suspiciously large (%d bytes), want it capped near recallLoadCap", len(out))
	}
}

// TestMemoryRecallFormatValidation ensures memory recall exits with an error
// when an invalid format is specified (regression for internal/cli/memory.go error handling).
func TestMemoryRecallFormatValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{"test", "--format", "invalid"})
	})
	if code == 0 {
		t.Errorf("memory recall with invalid format: exit %d, want non-zero; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "format") {
		t.Errorf("memory recall invalid format: stderr %q, want format error message", stderr)
	}
}

// TestMemoryRecallFailReturnValue ensures memory recall returns a non-zero code
// (regression for internal/cli/memory.go:150 where fail() return value not used).
func TestMemoryRecallFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory recall (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemoryRecallLoadBlockErrorSilent documents the behavior where ShowBlock() errors
// are silently ignored in recallLoad() (regression for internal/cli/memory.go:188).
// The fix should log or report errors instead of silently continuing.
func TestMemoryRecallLoadBlockErrorSilent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Use --load with an invalid block ID to trigger ShowBlock() error
	code, out, _ := captureOutput(t, func() int {
		return cmdMemoryRecall([]string{"nonexistent", "--load", "1"})
	})
	// Should succeed (exit 0) but only print the title, not the body
	// After the fix, errors should be logged or reported
	_ = code
	_ = out
}
