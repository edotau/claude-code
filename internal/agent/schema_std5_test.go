package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
)

func registeredStd5(t *testing.T, name string) {
	t.Helper()
	for _, f := range schemas.All() {
		if f.Name == name {
			return
		}
	}
	t.Errorf("schemas.All() has no %q row", name)
}

func writeRawDispatch(t *testing.T, id, body string) {
	t.Helper()
	if err := os.MkdirAll(DispatchDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(DispatchDir(), id+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDispatchRecordSchemaStd5 pins: unstamped record reads, new record is stamped 1, schema 2 is skipped/errors.
func TestDispatchRecordSchemaStd5(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	registeredStd5(t, "dispatch-record")

	const oldID, newerID = "20260101T000000.000-codex", "20260102T000000.000-codex"
	writeRawDispatch(t, oldID, `{"id":"`+oldID+`","time":"2026-01-01T00:00:00Z","agent":"codex","request":{},"status":"ok"}`)
	got, err := LoadRecord(oldID)
	if err != nil || got.ID != oldID || got.Status != "ok" {
		t.Fatalf("pre-schema record: %+v, %v", got, err)
	}

	rec := NewRecord("copilot", Request{}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(DispatchDir(), rec.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["schema"] != float64(1) {
		t.Errorf(`new record "schema" = %v, want 1`, raw["schema"])
	}

	writeRawDispatch(t, newerID, `{"schema":2,"id":"`+newerID+`","agent":"codex","status":"ok"}`)
	if _, err := LoadRecord(newerID); !errors.Is(err, schemas.ErrNewer) {
		t.Errorf("LoadRecord(schema 2) error = %v, want wrapping schemas.ErrNewer", err)
	}
	for _, r := range RecentRecords(10) {
		if r.ID == newerID {
			t.Errorf("RecentRecords listed the schema-2 record")
		}
	}
	if n := len(RecentRecords(10)); n != 2 {
		t.Errorf("RecentRecords = %d, want 2 (old + new)", n)
	}
}

func TestAskEnvelopeRegisteredStd5(t *testing.T) { registeredStd5(t, "ask-envelope") }
