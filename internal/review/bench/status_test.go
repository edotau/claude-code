package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	statusTestYear     = 2026
	statusTestTokens   = 100
	statusTestSavingMS = 400
	statusTestLane     = "bench-status-20261123-1"
)

func statusFixture(t *testing.T, lanes string) time.Time {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := laneList
	laneList = func(context.Context, string) (string, error) { return lanes, nil }
	t.Cleanup(func() { laneList = old })
	if err := os.MkdirAll(benchRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	statusWriteFixture(t, "candidates.jsonl", "")
	return time.Date(statusTestYear, 11, 23, 12, 0, 0, 0, time.UTC)
}

func statusWriteFixture(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(benchRoot(), name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func statusText(t *testing.T, s Status) string {
	t.Helper()
	var buf bytes.Buffer
	if err := s.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func statusWantText(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusBudgetLimit(t *testing.T) {
	now := statusFixture(t, "[]")
	var ledger bytes.Buffer
	for i := range dailyBenchLoops {
		row := budgetRow{At: now, Kind: KindBenchLoop, ID: strconv.Itoa(i), Tokens: statusTestTokens}
		if err := json.NewEncoder(&ledger).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	statusWriteFixture(t, "budget.jsonl", ledger.String())
	s := BuildStatus(t.Context(), t.TempDir(), now)
	wantTokens := dailyBenchLoops * statusTestTokens
	if s.Budget == nil || s.Budget.Tokens != wantTokens || s.Budget.Kinds[0].Allowed || !s.Budget.Kinds[1].Allowed {
		t.Fatalf("budget: %+v", s.Budget)
	}
	out := statusText(t, s)
	statusWantText(t, out, fmt.Sprintf("tokens: %d/%d today", wantTokens, dailyTokens), "bench-loop: 5/5 today",
		"bench-loop launch: refused: daily bench-loop count limit reached", "gate-fix: 0/6 today")
}

func TestStatusJSON(t *testing.T) {
	s := Status{Budget: &StatusBudget{Kinds: []StatusBudgetKind{{Kind: KindBenchLoop, Reason: "refused"}}}}
	var data bytes.Buffer
	if err := s.WriteJSON(&data); err != nil {
		t.Fatal(err)
	}
	var decoded Status
	if err := json.Unmarshal(data.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON round trip: %s (%v)", &data, err)
	}
	if decoded.Budget == nil || len(decoded.Budget.Kinds) != 1 || decoded.Budget.Kinds[0].Reason != "refused" {
		t.Fatalf("JSON lost budget: %+v", decoded.Budget)
	}
	if !strings.Contains(data.String(), "\n  \"budget\": {") {
		t.Fatal("JSON is not indented")
	}
}

func TestStatusCorruptBudget(t *testing.T) {
	now := statusFixture(t, "[]")
	statusWriteFixture(t, "budget.jsonl", "not json\n")
	s := BuildStatus(t.Context(), t.TempDir(), now)
	out := statusText(t, s)
	if s.Budget != nil || !strings.Contains(out, "budget: budget ledger line 1:") {
		t.Fatalf("corrupt budget: %+v\n%s", s, out)
	}
	if strings.Contains(out, "tokens:") || strings.Contains(out, "bench-loop: 0/") {
		t.Fatalf("corrupt ledger presented as zero use:\n%s", out)
	}
}

func TestStatusTextRows(t *testing.T) {
	name := statusTestLane
	lanes := fmt.Sprintf(`[{"Path":%q,"Branch":%q,"Behind":4}]`, "/lanes/"+name, "worktree-"+name)
	now := statusFixture(t, lanes)
	var candidates bytes.Buffer
	for i := range 4 {
		item := Item{Pkg: "internal/status", What: strconv.Itoa(i) + strings.Repeat(" long candidate", 20),
			CallsPerDay: 1, EstSavingMS: float64(statusTestSavingMS - i)}
		if err := json.NewEncoder(&candidates).Encode(item); err != nil {
			t.Fatal(err)
		}
	}
	statusWriteFixture(t, "candidates.jsonl", candidates.String())
	s := BuildStatus(t.Context(), t.TempDir(), now)
	if s.Queue == nil || s.Queue.Total != 4 || len(s.Queue.Top) != 3 || s.Queue.Counts["fix"] != 4 {
		t.Fatalf("queue: %+v (%s)", s.Queue, s.QueueError)
	}
	out := statusText(t, s)
	for row := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if len(row) > 80 {
			t.Errorf("%d columns: %s", len(row), row)
		}
	}
	statusWantText(t, out, "lanes: 1 open (cap 3)", name+": unmerged, dirty=false, behind=4",
		"claude-code worktree land "+name, "1. 0.40 s/day [fix]", "no fix item clears the 1 s/day loop floor")
}

func TestStatusLastLoop(t *testing.T) {
	now := statusFixture(t, "[]")
	dir := filepath.Join(benchRoot(), "post-commit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusWriteFixture(t, "post-commit/old.marker", `{"end":"2026-11-23T11:00:00Z","loop":"old"}`)
	statusWriteFixture(t, "post-commit/latest.marker", `{"loop":"budget refused"}`)
	statusWriteFixture(t, "post-commit/empty.marker", `{"end":"2026-11-23T12:00:00Z"}`)
	end := now.Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "latest.marker"), end, end); err != nil {
		t.Fatal(err)
	}
	s := BuildStatus(t.Context(), t.TempDir(), now)
	if s.LastLoop == nil || s.LastLoop.Reason != "budget refused" || s.LastLoop.AgeSeconds != 60 {
		t.Fatalf("last launch: %+v (%s)", s.LastLoop, s.LoopError)
	}
	out := statusText(t, s)
	if !strings.Contains(out, "loop: idle\nlast launch: 1m0s ago; budget refused") || strings.Contains(out, dir) {
		t.Fatalf("loop status:\n%s", out)
	}
}

func TestStatusReadOnlyUninitialized(t *testing.T) {
	now := statusFixture(t, "[]")
	if err := os.RemoveAll(benchRoot()); err != nil {
		t.Fatal(err)
	}
	s := BuildStatus(t.Context(), t.TempDir(), now)
	if s.QueueError == "" {
		t.Fatal("missing candidate ledger was not reported")
	}
	if _, err := os.Stat(benchRoot()); !os.IsNotExist(err) {
		t.Fatalf("status created bench state: %v", err)
	}
}
