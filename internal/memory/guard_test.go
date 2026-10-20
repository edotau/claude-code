package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardJournalCacheInvalidation(t *testing.T) {
	const complete = `{"phase":"complete","manifest":"/m/x.json"}`
	const applying = `{"phase":"applying","manifest":"/m/x.json"}`
	stamp := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		body      string
		offset    time.Duration
		replace   bool
		malformed bool
	}{
		{name: "mtime changed", body: applying, offset: time.Hour},
		{name: "size changed", body: applying + "\n"},
		{name: "replaced inode", body: applying, replace: true},
		{name: "malformed then fixed", body: "{", malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if err := os.MkdirAll(MigrateStateDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(MigrateStateDir(), "x.journal.json")
			writeJournal := func(path, body string, stamp time.Time) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			writeJournal(path, complete, stamp)
			for i := 0; i < 2; i++ {
				if !CutOver() {
					t.Fatalf("CutOver() on complete journal, read %d = false, want true", i+1)
				}
			}

			target := path
			if tc.replace {
				target = path + ".replacement"
			}
			writeJournal(target, tc.body, stamp.Add(tc.offset))
			if tc.replace {
				if err := os.Rename(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if CutOver() {
				t.Error("CutOver() after invalidation = true, want false")
			}
			if !tc.malformed {
				if g := StartupGuard(); !strings.Contains(g.Notice, "/m/x.json") {
					t.Errorf("StartupGuard().Notice = %q, want manifest /m/x.json", g.Notice)
				}
			}

			writeJournal(path, complete, stamp.Add(2*time.Hour))
			if !CutOver() {
				t.Error("CutOver() after repair = false, want true")
			}
		})
	}
}
