package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// summarizeFile totals every response in path uncached.
func summarizeFile(path string) (*FileSummary, error) {
	resps, err := ReadResponses(path)
	if err != nil {
		return nil, err
	}
	return SummarizeResponses(resps), nil
}

// addFile summarizes path uncached and folds it into agg.
func addFile(agg *UsageAggregate, path string) error {
	s, err := summarizeFile(path)
	if err != nil {
		return err
	}
	agg.Add(s)
	return nil
}

// wantModel returns models[name] after checking its input, output, cache-creation and cache-read totals.
func wantModel(t *testing.T, models map[string]*ModelUsage, name string, in, out, cacheRead, cacheCreate int64) *ModelUsage {
	t.Helper()
	m, ok := models[name]
	if !ok {
		t.Fatalf("missing %s totals", name)
	}
	if m.InputTokens != in || m.OutputTokens != out || m.CacheCreationTokens != cacheCreate || m.CacheReadTokens != cacheRead {
		t.Errorf("%s totals = %+v, want {%d %d %d %d}", name, m, in, out, cacheRead, cacheCreate)
	}
	return m
}

func TestUsageAggregateFixtures(t *testing.T) {
	agg := NewUsageAggregate()
	if err := addFile(agg, "testdata/usage_a.jsonl"); err != nil {
		t.Fatalf("AddFile(usage_a): %v", err)
	}
	if err := addFile(agg, "testdata/usage_b.jsonl"); err != nil {
		t.Fatalf("AddFile(usage_b): %v", err)
	}

	sonnet := wantModel(t, agg.Models, "claude-sonnet-5", 300, 30, 5, 50)
	wantModel(t, agg.Models, "claude-opus-4-8", 800, 55, 0, 0)

	// Peak sums input + cache writes per minute: 06:36 totals 650 and must win over 06:40's 500, scanned last.
	if agg.PeakInputTokensPerMin != 650 {
		t.Errorf("PeakInputTokensPerMin = %d, want 650", agg.PeakInputTokensPerMin)
	}

	if got := sonnet.Total(); got != 385 {
		t.Errorf("sonnet.Total() = %d, want 385", got)
	}
}

func TestUsageAggregateMissingFile(t *testing.T) {
	agg := NewUsageAggregate()
	if err := addFile(agg, "testdata/does-not-exist.jsonl"); err == nil {
		t.Error("AddFile on a missing file: want error, got nil")
	}
}

// summarizeFile is pinned against the fixture AddFile uses.
func TestSummarizeFileFixtures(t *testing.T) {
	s, err := summarizeFile("testdata/usage_a.jsonl")
	if err != nil {
		t.Fatalf("summarizeFile(usage_a): %v", err)
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
	wantModel(t, s.Models, "claude-sonnet-5", 300, 30, 5, 50)
	tot := s.Totals()
	if got := tot.Total(); got != 385 {
		t.Errorf("Totals().Total() = %d, want 385", got)
	}
}

// Absent timestamps give a zero span so the WALL column prints "-", never a 1970-based duration.
func TestSummarizeFileNoTimestamps(t *testing.T) {
	s, err := summarizeFile("testdata/usage_no_timestamps.jsonl")
	if err != nil {
		t.Fatalf("summarizeFile: %v", err)
	}
	if s.Turns != 1 {
		t.Errorf("Turns = %d, want 1", s.Turns)
	}
	if !s.First.IsZero() || !s.Last.IsZero() {
		t.Errorf("span = [%s, %s], want zero/zero", s.First, s.Last)
	}
}

// usage_dedup.jsonl's first four lines (real, 2026-09-26) share one id: last wins, input 2 output 247, not 8/262.
func TestSummarizeFileDedupesByMessageID(t *testing.T) {
	s, err := summarizeFile("testdata/usage_dedup.jsonl")
	if err != nil {
		t.Fatalf("summarizeFile(usage_dedup): %v", err)
	}
	if s.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (one deduped response + one standalone no-id line)", s.Turns)
	}
	sonnet, ok := s.Models["claude-sonnet-5"]
	if !ok {
		t.Fatal("missing claude-sonnet-5 in file summary")
	}
	// Deduped response plus the standalone no-id line (7/1/2/3) sum to 9/23880/2/250.
	if sonnet.InputTokens != 9 || sonnet.OutputTokens != 250 || sonnet.CacheCreationTokens != 23880 || sonnet.CacheReadTokens != 2 {
		t.Errorf("sonnet totals = %+v, want {9 250 2 23880}", sonnet)
	}
}

// EachLine has no per-line cap, so a >16 MB line must not truncate or abort the scan around it.
func TestSummarizeFileOversizedLineDoesNotAbortScan(t *testing.T) {
	huge := strings.Repeat("a", 16*1024*1024+1024)
	line1 := `{"type":"assistant","timestamp":"2026-07-19T06:36:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_before","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}`
	// An unread "filler" field pads the line past the old 16 MB scanner cap; its usage must still count.
	line2 := fmt.Sprintf(`{"type":"assistant","timestamp":"2026-07-19T06:37:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_huge","usage":{"input_tokens":11,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2},"filler":%q}}`, huge)
	line3 := `{"type":"assistant","timestamp":"2026-07-19T06:38:00.000Z","message":{"model":"claude-sonnet-5","id":"msg_after","usage":{"input_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}`

	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	content := strings.Join([]string{line1, line2, line3}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	s, err := summarizeFile(path)
	if err != nil {
		t.Fatalf("summarizeFile: %v", err)
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

// writeTree creates each path under root (parents included) with content.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A session's run dirs skip stray files; a run's agent files skip the .meta.json sidecars.
func TestWorkflowRunDirsAndAgentFiles(t *testing.T) {
	session := t.TempDir()
	writeTree(t, session, map[string]string{
		"subagents/workflows/wf_abc/agent-one.jsonl":     "",
		"subagents/workflows/wf_abc/agent-one.meta.json": `{"workflowPhase":"Detect"}`,
		"subagents/workflows/wf_abc/agent-two.jsonl":     "",
		"subagents/workflows/stray.txt":                  "",
	})
	run := filepath.Join(session, "subagents", "workflows", "wf_abc")
	if dirs := WorkflowRunDirs(session); !slices.Equal(dirs, []string{run}) {
		t.Fatalf("WorkflowRunDirs = %v, want [%s]", dirs, run)
	}
	want := []string{filepath.Join(run, "agent-one.jsonl"), filepath.Join(run, "agent-two.jsonl")}
	if files := WorkflowAgentFiles(run); !slices.Equal(files, want) {
		t.Errorf("WorkflowAgentFiles = %v, want %v", files, want)
	}
}

func TestWindow(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "claude-opus-5[1m]")
	for _, c := range []struct {
		model string
		used  int
		want  int
	}{{"claude-opus-5", 10, 1_000_000}, {"claude-haiku-4-5", 10, 200_000}, {"claude-haiku-4-5", 300_000, 200_000}, {"", 300_000, 1_000_000}, {"claude-opus-4-8[1m]", 1, 1_000_000}, {"claude-opus-4-8", 1, 200_000}} {
		if got := Window(c.model, c.used); got != c.want {
			t.Errorf("Window(%q,%d)=%d want %d", c.model, c.used, got, c.want)
		}
	}
}

// A Databricks pin "databricks-<id>[1m]" must cover the bare id the transcript records.
func TestWindowDatabricksPrefixedPin(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "databricks-claude-opus-4-8[1m]")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "")
	if got := Window("claude-opus-4-8", 10); got != 1_000_000 {
		t.Errorf("Window(claude-opus-4-8)=%d want 1000000", got)
	}
	if got := Window("claude-haiku-4-5", 10); got != 200_000 {
		t.Errorf("Window(claude-haiku-4-5)=%d want 200000 (unpinned model)", got)
	}
}

// A pin on another vendor's "vendor-<id>[1m]" must NOT hand the bare id a 1M window (only "databricks-" is stripped).
func TestWindowNonDatabricksPrefixedPinNotMatched(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "vendor-claude-opus-4-8[1m]")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "")
	if got := Window("claude-opus-4-8", 10); got == 1_000_000 {
		t.Errorf("Window(claude-opus-4-8)=%d: a vendor- pin must not match the bare id", got)
	}
	t.Setenv("ANTHROPIC_MODEL", "databricks-claude-opus-4-8[1m]")
	if got := Window("claude-opus-4-8", 10); got != 1_000_000 {
		t.Errorf("databricks- pin must still match: %d", got)
	}
}

func TestWindowDisable1MContext(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_1M_CONTEXT", "1")
	if got := Window("claude-opus-5-5", 10); got != 200_000 {
		t.Errorf("Window under DISABLE_1M_CONTEXT = %d, want 200000", got)
	}
}

func TestGlobInMatchesMetacharacterDirsLiterally(t *testing.T) {
	name := "a[b]c" // `*` and `?` cannot be file names on Windows, `[` can
	if filepath.Separator != '\\' {
		name += `?*\` // elsewhere `\` is an ordinary name character and is escaped too
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(want, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := GlobIn(dir, "*.jsonl"); !slices.Equal(got, []string{want}) {
		t.Errorf("GlobIn(%q) = %v, want [%s]", dir, got, want)
	}
	if got := GlobLiteral(`\`); (filepath.Separator == '\\') != (got == `\`) {
		t.Errorf("GlobLiteral(`\\`) = %q on separator %q", got, filepath.Separator)
	}
}
