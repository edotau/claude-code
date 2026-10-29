package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeJournal seeds a pending one-file journal (extra fields merged in) under the state journal dir.
func writeJournal(t *testing.T, id string, extra map[string]any) {
	t.Helper()
	doc := map[string]any{
		"id": id, "subject": "seeded", "state": "pending",
		"changes": []map[string]any{{"rel": "r/seeded.md", "delete": false, "hasPreimage": false, "data": []byte("seeded\n")}},
	}
	for k, v := range extra {
		doc[k] = v
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(JournalDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(JournalDir(), id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nextBatch(t *testing.T) error {
	t.Helper()
	_, err := commitLocked(t, "next", []Change{{Rel: "r/next.md", Data: []byte("n\n")}})
	return err
}

// A schema-2 (newer) journal is never replayed: its target stays untouched and the file stays in place.
func TestRecoverSkipsNewerSchemaJournal(t *testing.T) {
	store, decoy := storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	id := "20260101-000000.000-aaaa"
	writeJournal(t, id, map[string]any{"schema": 2})

	_ = nextBatch(t) // reporting the newer journal as an error is allowed; replaying it is not
	if _, err := os.Stat(filepath.Join(store, "r", "seeded.md")); !os.IsNotExist(err) {
		t.Fatalf("newer journal was replayed (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(JournalDir(), id+".json")); err != nil {
		t.Fatalf("newer journal not left in place: %v", err)
	}
	assertDecoyUntouched(t, decoy)
}

// A v0 journal (no schema field) still recovers on the next batch.
func TestRecoverReplaysV0Journal(t *testing.T) {
	storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, "20260101-000000.000-bbbb", nil)

	if err := nextBatch(t); err != nil {
		t.Fatal(err)
	}
	if got := readStore(t, "r/seeded.md"); got != "seeded\n" {
		t.Fatalf("v0 journal not recovered: %q", got)
	}
	if len(journals(t)) != 0 {
		t.Fatalf("journal not retired: %v", journals(t))
	}
}
