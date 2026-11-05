package project

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRowSinkClassifiesLines(t *testing.T) {
	var s rowSink
	in := "== codex ==\n  OK    ~/.codex/agents/foo.toml (unchanged)\n  DRY   ~/.codex/agents/bar.toml\n" +
		"  KEEP  ~/.codex/AGENTS.md (hand-authored; remove it)\n== skills ==\n" +
		"  EXTRA ~/.agents/skills/x (synced skills go to no agent; preserved)\n  LINK  ~/.agents/skills/s -> ~/.claude/skills/s\n"
	s.Write([]byte(in[:40]))
	s.Write([]byte(in[40:]))
	want := []SyncRow{
		{"codex", "foo", "agent", "~/.codex/agents/foo.toml", StateCurrent, "unchanged", "unverified"},
		{"codex", "bar", "agent", "~/.codex/agents/bar.toml", StateStale, "", "unverified"},
		{"codex", "AGENTS", "instructions", "~/.codex/AGENTS.md", StateBlocked, "hand-authored; remove it", "unverified"},
		{"skills", "x", "extra", "~/.agents/skills/x", StatePreserved, "synced skills go to no agent; preserved", "unverified"},
		{"skills", "s", "skill", "~/.agents/skills/s", StateStale, "", "unverified"},
	}
	if len(s.rows) != len(want) {
		t.Fatalf("rows = %+v", s.rows)
	}
	for i, w := range want {
		if s.rows[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, s.rows[i], w)
		}
	}
}

func TestWriteRowsEmptyIsArrayAndKeepsCode(t *testing.T) {
	var b bytes.Buffer
	if code := writeRows(&b, nil, 1); code != 1 {
		t.Fatalf("code = %d", code)
	}
	var got struct{ Rows []SyncRow }
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || got.Rows == nil {
		t.Fatalf("%v %s", err, b.String())
	}
}

// Given an unsynced source, --check --format json emits one document with a stale row and exits 1.
func TestSyncJSONCheckReportsStaleRows(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	var out bytes.Buffer
	if code := Sync(Options{JSON: true, Check: true, Agent: "copilot", Out: &out}); code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out.String())
	}
	var got struct{ Rows []SyncRow }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out.String())
	}
	for _, r := range got.Rows {
		if r.Name == "demo" && r.State == StateStale && r.Runtime == "unverified" {
			return
		}
	}
	t.Fatalf("no stale demo row: %+v", got.Rows)
}
