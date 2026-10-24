package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if fi, _ := os.Stat(filepath.Join(DispatchDir(), r.ID+".json")); fi.Mode().Perm() != 0o600 {
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

// Repo/PID/Log round-trip through JSON (and are omitted when zero).
func TestRecordJSONRoundTripsRepoPIDLog(t *testing.T) {
	r := Record{ID: "x", Agent: "codex", Repo: "/repo", PID: 4242, Log: "/repo/x.log"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"repo":"/repo"`) || !strings.Contains(s, `"pid":4242`) || !strings.Contains(s, `"log":"/repo/x.log"`) {
		t.Fatalf("marshaled record missing repo/pid/log: %s", s)
	}
	var got Record
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Repo != r.Repo || got.PID != r.PID || got.Log != r.Log {
		t.Fatalf("round trip = %+v, want %+v", got, r)
	}

	zero := Record{ID: "y"}
	zb, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(zb); strings.Contains(s, `"repo"`) || strings.Contains(s, `"pid"`) || strings.Contains(s, `"log"`) {
		t.Fatalf("zero-valued repo/pid/log must be omitted: %s", s)
	}
}

// pruneDispatches removes the pruned record's .log alongside its .json.
func TestPruneDispatchesRemovesLog(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	old := dispatchKeep
	dispatchKeep = 1
	defer func() { dispatchKeep = old }()

	first := NewRecord("codex", Request{Prompt: "p"}, base)
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(DispatchDir(), first.ID+".log")
	if err := os.WriteFile(logPath, []byte("job output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log must exist before prune: %v", err)
	}

	// second record pushes the first past dispatchKeep=1, triggering prune
	if err := NewRecord("codex", Request{Prompt: "q"}, base.Add(time.Minute)).Save(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("pruned record's log must be removed, stat err = %v", err)
	}
}
