package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A record saves as running, finishes with its outcome, loads by prefix, and the log prunes the oldest.
func TestDispatchRecordLifecycleAndPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	r := NewRecord("codex", Request{Prompt: "p", Tools: true}, base)
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadRecord("20261016T210000"); got.Status != "running" || got.Request.Prompt != "p" || !got.Request.Tools {
		t.Fatalf("running record = %+v", got)
	}
	r.Finish(Result{Provider: "chatgpt"}, nil, time.Second)
	_ = r.Save()
	if got, _ := LoadRecord(r.ID); got.Status != "ok" || got.Provider != "chatgpt" {
		t.Fatalf("finished record = %+v", got)
	}
	if fi, _ := os.Stat(filepath.Join(dispatchDir(), r.ID+".json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	old := dispatchKeep
	dispatchKeep = 2
	defer func() { dispatchKeep = old }()
	for i := 1; i <= 3; i++ {
		_ = NewRecord("gemini", Request{Prompt: "q"}, base.Add(time.Duration(i)*time.Minute)).Save()
	}
	if recs := RecentRecords(10); len(recs) != 2 || recs[0].Time.Before(recs[1].Time) {
		t.Fatalf("after prune: %d records, newest first = %v", len(recs), recs)
	}
	if _, err := LoadRecord("2026"); err == nil {
		t.Error("an ambiguous prefix must not load")
	}
}
