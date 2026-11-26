package project

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The same status calls print the text lines and, into a rowSink, the matching rows.

func TestWriteRowsEmptyIsArrayAndKeepsCode(t *testing.T) {
	var b bytes.Buffer
	if code := writeRows(&b, nil, 1); code != 1 {
		t.Fatalf("code = %d", code)
	}
	var got struct{ Rows []syncRow }
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
	var got struct{ Rows []syncRow }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out.String())
	}
	for _, r := range got.Rows {
		if r.Name == "demo" && r.State == stateStale && r.Runtime == "unverified" {
			return
		}
	}
	t.Fatalf("no stale demo row: %+v", got.Rows)
}
