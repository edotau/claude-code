package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageAggregateFixtures(t *testing.T) {
	agg := NewUsageAggregate()
	if err := agg.AddFile("testdata/usage_a.jsonl"); err != nil {
		t.Fatalf("AddFile(usage_a): %v", err)
	}
	if err := agg.AddFile("testdata/usage_b.jsonl"); err != nil {
		t.Fatalf("AddFile(usage_b): %v", err)
	}

	sonnet, ok := agg.Models["claude-sonnet-5"]
	if !ok {
		t.Fatal("missing claude-sonnet-5 totals")
	}
	if sonnet.InputTokens != 300 || sonnet.OutputTokens != 30 || sonnet.CacheCreationTokens != 50 || sonnet.CacheReadTokens != 5 {
		t.Errorf("sonnet totals = %+v, want {300 30 5 50}", sonnet)
	}

	opus, ok := agg.Models["claude-opus-4-8"]
	if !ok {
		t.Fatal("missing claude-opus-4-8 totals")
	}
	if opus.InputTokens != 800 || opus.OutputTokens != 55 || opus.CacheCreationTokens != 0 || opus.CacheReadTokens != 0 {
		t.Errorf("opus totals = %+v, want {800 55 0 0}", opus)
	}

	// Peak per-minute bucket sums uncached input (input_tokens + cache_creation_input_tokens), not input
	// alone: minute 06:36 sums (100+50)+(200+0)[A] + (300+0)[B] = 650; minute 06:40 sums 500+0=500[B] alone.
	// 650 must win, not the 500 in the file scanned last.
	if agg.PeakInputTokensPerMin != 650 {
		t.Errorf("PeakInputTokensPerMin = %d, want 650", agg.PeakInputTokensPerMin)
	}

	if got := sonnet.Total(); got != 385 {
		t.Errorf("sonnet.Total() = %d, want 385", got)
	}
}

func TestUsageAggregateMissingFile(t *testing.T) {
	agg := NewUsageAggregate()
	if err := agg.AddFile("testdata/does-not-exist.jsonl"); err == nil {
		t.Error("AddFile on a missing file: want error, got nil")
	}
}

// TestSummarizeFileFixtures pins SummarizeFile against the same fixture AddFile is pinned against
// (usage --workflows reuses this helper for per-run/per-phase wall time and turn counts).
func TestSummarizeFileFixtures(t *testing.T) {
	s, err := SummarizeFile("testdata/usage_a.jsonl")
	if err != nil {
		t.Fatalf("SummarizeFile(usage_a): %v", err)
	}
	// usage_a has 2 assistant records with usage (the 3rd line is malformed, the 4th is a user record).
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2", s.Turns)
	}
	wantFirst := time.Date(2026, 7, 19, 6, 36, 0, 0, time.UTC)
	wantLast := time.Date(2026, 7, 19, 6, 36, 30, 0, time.UTC)
	if !s.First.Equal(wantFirst) || !s.Last.Equal(wantLast) {
		t.Errorf("span = [%s, %s], want [%s, %s]", s.First, s.Last, wantFirst, wantLast)
	}
	sonnet, ok := s.Models["claude-sonnet-5"]
	if !ok {
		t.Fatal("missing claude-sonnet-5 in file summary")
	}
	if sonnet.InputTokens != 300 || sonnet.OutputTokens != 30 || sonnet.CacheCreationTokens != 50 || sonnet.CacheReadTokens != 5 {
		t.Errorf("sonnet totals = %+v, want {300 30 5 50}", sonnet)
	}
	tot := s.Totals()
	if got := tot.Total(); got != 385 {
		t.Errorf("Totals().Total() = %d, want 385", got)
	}
}

// TestSummarizeFileNoTimestamps confirms a file with unparseable/absent timestamps degrades to a
// zero span rather than erroring — the WALL column must print "-", never a 1970-based duration.
func TestSummarizeFileNoTimestamps(t *testing.T) {
	s, err := SummarizeFile("testdata/usage_no_timestamps.jsonl")
	if err != nil {
		t.Fatalf("SummarizeFile: %v", err)
	}
	if s.Turns != 1 {
		t.Errorf("Turns = %d, want 1", s.Turns)
	}
	if !s.First.IsZero() || !s.Last.IsZero() {
		t.Errorf("span = [%s, %s], want zero/zero", s.First, s.Last)
	}
}

func TestSummarizeFileMissingFile(t *testing.T) {
	if _, err := SummarizeFile("testdata/does-not-exist.jsonl"); err == nil {
		t.Error("SummarizeFile on a missing file: want error, got nil")
	}
}

// TestSummarizeFileDedupesByMessageID pins the dbx-bug fix: Claude Code writes one JSONL line per
// content block of an API response, each repeating message.usage under the same message.id, and
// output_tokens grows across a response's lines while streaming. testdata/usage_dedup.jsonl's first
// four lines are real records sampled 2026-09-26 (trimmed) sharing one id — a naive per-line sum would
// give input 8, output 262; the last-line-wins fix must give input 2, output 247 for that response. The
// fifth line is synthetic (no message.id) and must count as its own, separate response.
func TestSummarizeFileDedupesByMessageID(t *testing.T) {
	s, err := SummarizeFile("testdata/usage_dedup.jsonl")
	if err != nil {
		t.Fatalf("SummarizeFile(usage_dedup): %v", err)
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (one deduped response + one standalone no-id line)", s.Turns)
	}
	sonnet, ok := s.Models["claude-sonnet-5"]
	if !ok {
		t.Fatal("missing claude-sonnet-5 in file summary")
	}
	// Deduped response (last line wins): input 2, cache_creation 23879, cache_read 0, output 247.
	// Standalone no-id line: input 7, cache_creation 1, cache_read 2, output 3. Summed: 9/23880/2/250.
	if sonnet.InputTokens != 9 || sonnet.OutputTokens != 250 || sonnet.CacheCreationTokens != 23880 || sonnet.CacheReadTokens != 2 {
		t.Errorf("sonnet totals = %+v, want {9 250 2 23880}", sonnet)
	}
}

// TestSummarizeFileOversizedLineDoesNotAbortScan: transcript's eachLine has no per-line cap (an
// overflow buffer joins arbitrarily long lines), so a >16 MB line — legal JSON, just huge — must not
// truncate or abort the scan of the valid lines before and after it.
func TestSummarizeFileOversizedLineDoesNotAbortScan(t *testing.T) {
	huge := strings.Repeat("a", 16*1024*1024+1024)
	line1 := `{"type":"assistant","timestamp":"2026-07-19T06:36:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_before","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	// A single legal (if oversized) assistant JSONL line: an extra, unread "filler" field pads it past
	// the old 16 MB scanner cap, and its own usage still must be counted.
	line2 := fmt.Sprintf(`{"type":"assistant","timestamp":"2026-07-19T06:37:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_huge","usage":{"input_tokens":11,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2},"filler":%q}}`, huge)
	line3 := `{"type":"assistant","timestamp":"2026-07-19T06:38:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_after","usage":{"input_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}`

	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	content := strings.Join([]string{line1, line2, line3}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s, err := SummarizeFile(path)
	if err != nil {
		t.Fatalf("SummarizeFile: %v", err)
	}
	if s.Turns != 3 {
		t.Fatalf("Turns = %d, want 3 (the oversized line must not abort the scan of the line after it)", s.Turns)
	}
	sonnet, ok := s.Models["claude-sonnet-5"]
	if !ok {
		t.Fatal("missing claude-sonnet-5 in file summary")
	}
	if want := int64(5 + 11 + 9); sonnet.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d (before + huge + after all counted)", sonnet.InputTokens, want)
	}
}
