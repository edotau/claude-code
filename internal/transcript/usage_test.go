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

// addFile summarizes path uncached and folds it into agg.
func addFile(agg *UsageAggregate, path string) error {
	s, err := SummarizeFile(path)
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
	if err := addFile(agg, "testdata/does-not-exist.jsonl"); err == nil {
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
	wantModel(t, s.Models, "claude-sonnet-5", 300, 30, 5, 50)
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

// TestSummarizeFileDedupesByMessageID pins the dedupe: Claude Code writes one JSONL line per
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

// TestSummarizeFileOversizedLineDoesNotAbortScan: transcript's EachLine has no per-line cap (an
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

// The sidecar's phase is read; a missing sidecar is a silent "", a malformed one "" with a warning.
func TestWorkflowPhaseFromMeta(t *testing.T) {
	for _, tc := range []struct {
		name, meta, want string
		warn             bool
	}{
		{"phase", `{"workflowPhase":"Detect"}`, "Detect", false},
		{"no sidecar", "", "", false},
		{"malformed", `{"workflowPhase":`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{"agent-a.jsonl": ""}
			if tc.meta != "" {
				files["agent-a.meta.json"] = tc.meta
			}
			writeTree(t, dir, files)
			var errw strings.Builder
			got := WorkflowPhaseFromMeta(filepath.Join(dir, "agent-a.jsonl"), &errw)
			if warned := strings.Contains(errw.String(), "usage: warning: skipping meta for"); got != tc.want || warned != tc.warn {
				t.Errorf("phase = %q, warning %q; want %q, warning=%v", got, errw.String(), tc.want, tc.warn)
			}
		})
	}
}
