package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// A cut-off run is filed under its task hash, an hour-old entry is pruned on the next write, and a success clears it.
func TestNoteCutoffWritesPrunesAndClears(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mk := func(id, prompt string) Record {
		return Record{ID: id, Agent: "codex", Request: RunOptions{Model: "m", Prompt: prompt}}
	}
	old, fresh := mk("old", "a"), mk("fresh", "b\n\n<memory-recall>\n- x")
	old.Finish(wire.Result{}, ErrStalled, 0)
	fresh.Finish(wire.Result{}, ErrStalled, 0)
	if err := old.NoteCutoff(now.Add(-61 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := fresh.NoteCutoff(now); err != nil {
		t.Fatal(err)
	}
	if got := readCutoffs(); len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("entries %+v, want only fresh (old pruned)", got)
	}
	key := CutoffKey("codex", "m", "b  \n") // recall block and trailing whitespace are not part of the task
	if e, ok := RecentCutoff(key, now.Add(59*time.Minute)); !ok || e.Kind != CutoffStall {
		t.Fatalf("recent = %+v %v, want the stall entry", e, ok)
	}
	if _, ok := RecentCutoff(key, now.Add(61*time.Minute)); ok {
		t.Fatal("an entry past 60 minutes still matched")
	}
	ok := mk("later", "b")
	ok.Finish(wire.Result{}, nil, 0)
	if err := ok.NoteCutoff(now); err != nil {
		t.Fatal(err)
	}
	if got := readCutoffs(); len(got) != 0 {
		t.Fatalf("entries %+v after a success, want none", got)
	}
}

// A cutoff of a "--model auto" run is found by the key the guard computes from the typed word.
func TestCutoffKeyAutoMatchesFoldedRecord(t *testing.T) {
	rec := Record{Agent: "codex", Request: RunOptions{Model: "", AutoModel: true, Prompt: "p"}}
	for _, typed := range []string{"auto", "AUTO", ""} {
		if CutoffKey("codex", typed, "p") != CutoffKey(rec.Agent, rec.Request.Model, rec.Request.Prompt) {
			t.Fatalf("key for typed %q differs from the folded record's", typed)
		}
	}
	if CutoffKey("codex", "opus", "p") == CutoffKey("codex", "", "p") {
		t.Fatal("a real model shares the auto key")
	}
}

// A success clears only the entry it saw: a newer cutoff of the same task, written since, stays.
func TestClearCutoffKeepsNewerEntry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	r := Record{ID: "newer", Agent: "codex", Request: RunOptions{Model: "m", Prompt: "a"}}
	r.Finish(wire.Result{}, ErrStalled, 0)
	if err := r.NoteCutoff(now); err != nil {
		t.Fatal(err)
	}
	key := CutoffKey("codex", "m", "a")
	if err := clearCutoff(key, "older", now); err != nil {
		t.Fatal(err)
	}
	if got := readCutoffs(); len(got) != 1 || got[0].ID != "newer" {
		t.Fatalf("entries %+v, want the newer cutoff kept", got)
	}
}
