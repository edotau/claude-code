package router

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
)

func std10Seed(t *testing.T, body string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(StatePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStd10V0QuotaStaysReadableByOldBareMapReader: after Update on a v0 file, an old bare-map reader sees the entries.
func TestStd10V0QuotaStaysReadableByOldBareMapReader(t *testing.T) {
	std10Seed(t, `{"acct-a":{"used_pct":42,"seen":"2026-01-01T00:00:00Z"}}`)
	if err := Update(func(s State) { s["acct-b"] = Entry{UsedPct: 7, Seen: time.Now()} }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]Entry // the pre-schema daemon's decoder
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("old bare-map reader fails: %v\n%s", err, raw)
	}
	if e, ok := old["acct-a"]; !ok || e.UsedPct != 42 {
		t.Fatalf("old reader lost the original entry: %+v\n%s", old, raw)
	}
}

// TestStd10NewerQuotaSchemaIsPreserved: Update on a schema-2 file errors with ErrNewer and leaves the bytes alone.
func TestStd10NewerQuotaSchemaIsPreserved(t *testing.T) {
	body := `{"schema":2,"accounts":{"a":{"used_pct":1,"seen":"2026-01-01T00:00:00Z"}},"future":true}`
	std10Seed(t, body)
	err := Update(func(s State) { s["x"] = Entry{UsedPct: 9} })
	if !errors.Is(err, schemas.ErrNewer) {
		t.Fatalf("Update err = %v, want ErrNewer", err)
	}
	if got, _ := os.ReadFile(StatePath()); string(got) != body {
		t.Fatalf("newer-schema file was rewritten:\n%s", got)
	}
}
