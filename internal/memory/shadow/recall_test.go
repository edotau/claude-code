package shadow

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

func recallStore(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "0")
	root := paths.MemoryStoreDir()
	writeShadowStore(t, root, false)
	files := map[string]string{
		"native.md":      "---\nname: native top\ndescription: zebrafish storage\n---\n# Top-level native\n\nagent policy\n",
		"conventions.md": "# Conventions\n\n- Subagent zebrafish storage policy <!-- id:aaaa0001 -->\n\n- Agent storage details <!-- id:aaaa0002 -->\n",
		"notes/x.md":     "---\nname: native\ndescription: zebrafish policy\n---\n# Native\n\nagent storage body\n",
		"knowledge/k.md": "---\nname: knowledge\n---\n# Zebrafish knowledge\n\nagent storage policy\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ageRecallFiles(t, root)
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	seedRecallMeta(t, root)
	return root
}

func seedRecallMeta(t *testing.T, root string) {
	t.Helper()
	files, ok := recallFiles(root)
	if !ok {
		t.Fatal("recall files are racy")
	}
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	execShadow(t, db, "DELETE FROM meta WHERE key LIKE 'stat:%' OR key IN ('entries', 'imported_schema')")
	for path, stamp := range files {
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)`, "stat:"+path, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES('imported_schema', ?),
		('entries', (SELECT count(*) FROM entries WHERE deleted = 0))`, importMarker()); err != nil {
		t.Fatal(err)
	}
}

func recallStamps(t *testing.T) map[string]string {
	t.Helper()
	files, ok := recallFiles(paths.MemoryStoreDir())
	if !ok {
		t.Fatal("recallFiles refused the test store")
	}
	return files
}

func recallMarkdown(t *testing.T, query string, k int) ([]memory.SearchHit, string, string) {
	t.Helper()
	rowRecall := memory.RowRecall
	memory.RowRecall = nil
	defer func() { memory.RowRecall = rowRecall }()
	hits := memory.SearchBank(query, k)
	var report, warnings bytes.Buffer
	memory.RecallReport(&report, &warnings, memory.RecallBank(query, k), 0)
	block, _ := memory.AgentRecall(context.Background(), "", query)
	return hits, report.String(), block
}

// wantRecallParity checks row and Markdown output match and returns how many recall calls the rows served.
func wantRecallParity(t *testing.T, query string, k int) int {
	t.Helper()
	want, report, block := recallMarkdown(t, query, k)
	rowRecall, served := memory.RowRecall, 0
	memory.RowRecall = func(terms []string) (map[string][]byte, bool) {
		docs, ok := rowRecall(terms)
		if ok {
			served++
		}
		return docs, ok
	}
	defer func() { memory.RowRecall = rowRecall }()
	if err := os.Remove(filepath.Join(paths.MemoryStateDir(), "recall.idx")); err != nil && !os.IsNotExist(err) { // cold index: rows serve
		t.Fatal(err)
	}
	if got := memory.SearchBank(query, k); !reflect.DeepEqual(got, want) {
		t.Fatalf("SearchBank(%q, %d) = %+v, want %+v", query, k, got, want)
	}
	var got, warnings bytes.Buffer
	memory.RecallReport(&got, &warnings, memory.RecallBank(query, k), 0)
	if got.String() != report {
		t.Fatalf("RecallBank(%q) = %q, want %q", query, got.String(), report)
	}
	if got, _ := memory.AgentRecall(context.Background(), "", query); got != block {
		t.Fatalf("AgentRecall(%q) = %q, want %q", query, got, block)
	}
	return served
}

func TestRecallRowsParity(t *testing.T) {
	recallStore(t)
	docs, ok := recallRows([]string{"agent"})
	if !ok || len(docs) == 0 || docs["conventions.md"] == nil {
		t.Fatalf("row recall did not run: docs=%d ok=%v", len(docs), ok)
	}
	for _, query := range []string{"zebrafish storage", "agent", "fish", "ative", "archived", "native", "zebrafish agent storage policy", "missing", "", `" OR *`} {
		for _, k := range []int{0, 1, 3} {
			if served := wantRecallParity(t, query, k); served == 0 && strings.ContainsAny(query, "abcdefghijklmnopqrstuvwxyz") {
				t.Fatalf("row path never served %q", query)
			}
		}
	}
}

func TestRecallRowsStale(t *testing.T) {
	root := recallStore(t)
	p := filepath.Join(root, "conventions.md")
	if err := os.WriteFile(p, []byte("# Conventions\n\n- Changed zebrafish storage policy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := recallRows([]string{"zebrafish"}); ok {
		t.Fatal("stale database accepted")
	}
	wantRecallParity(t, "changed zebrafish storage policy", 0)
}

func TestRecallRowsMissing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := paths.MemoryStoreDir()
	writeShadowStore(t, root, false)
	if _, ok := recallRows([]string{"zebrafish"}); ok {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(filepath.Dir(ShadowDBPath())); !os.IsNotExist(err) {
		t.Fatalf("missing database created state directory: %v", err)
	}
	wantRecallParity(t, "zebrafish storage body speed", 0)
}

func TestRecallRowsCount(t *testing.T) {
	recallStore(t)
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := shadowRebuild(context.Background(), db, paths.MemoryStoreDir()); err != nil {
		t.Fatal(err)
	}
	execShadow(t, db, "DELETE FROM entries WHERE rowid = (SELECT min(rowid) FROM entries)")
	if recallCurrent(context.Background(), db, recallStamps(t)) {
		t.Fatal("database with a missing entry accepted")
	}
}

func TestRecallRowsFingerprint(t *testing.T) {
	root := recallStore(t)
	for _, rel := range []string{"notes/added.md", "reports/added.md", "handoffs/added.md", "worktrees/added.md", "archive/added.md"} {
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			p := filepath.Join(root, rel)
			if err := os.WriteFile(p, []byte("new content\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok := recallRows([]string{"new"}); ok {
				t.Fatal("added in-scope file did not invalidate fingerprint")
			}
			ageRecallFiles(t, root)
			if _, err := RebuildShadow(context.Background()); err != nil {
				t.Fatal(err)
			}
			seedRecallMeta(t, root)
		})
	}
}

func TestRecallRowsCurrentness(t *testing.T) {
	for _, change := range []string{"current", "touched", "new", "removed", "racy"} {
		t.Run(change, func(t *testing.T) {
			root := recallStore(t)
			changeRecallFile(t, root, change)
			_, ok := recallRows([]string{"zebrafish"})
			if ok != (change == "current") {
				t.Fatalf("current = %v after %s", ok, change)
			}
		})
	}
}

func changeRecallFile(t *testing.T, root, change string) {
	t.Helper()
	p := filepath.Join(root, "conventions.md")
	var err error
	switch change {
	case "touched":
		stamp := time.Now().Add(-3 * paths.RacyWindow)
		err = os.Chtimes(p, stamp, stamp)
	case "new":
		err = os.WriteFile(filepath.Join(root, "notes", "new.md"), []byte("new"), 0o600)
	case "removed":
		err = os.Remove(p)
	case "racy":
		err = os.Chtimes(p, time.Now(), time.Now())
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecallRowsDamaged(t *testing.T) {
	for _, query := range []string{
		"DELETE FROM meta WHERE key = 'entries'",
		"DROP TABLE meta",
		"UPDATE entries SET deleted = 1 WHERE rowid = (SELECT min(rowid) FROM entries)",
	} {
		t.Run(query, func(t *testing.T) {
			recallStore(t)
			db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			execShadow(t, db, query)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, ok := recallRows([]string{"zebrafish"}); ok {
				t.Fatal("damaged database accepted")
			}
			wantRecallParity(t, "zebrafish agent storage policy", 0)
		})
	}
}

func TestRecallRowsTextVariants(t *testing.T) {
	root := recallStore(t)
	body := "# Conventions\n\n- Café naïve coöperate l'été snake_case agent-router abc123 <!-- id:aaaa0001 -->\n\n- Ελληνικά 中文 русский cafe\u0301\n"
	for _, newline := range []string{"\n", "\r\n"} {
		if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte(strings.ReplaceAll(body, "\n", newline)), 0o600); err != nil {
			t.Fatal(err)
		}
		ageRecallFiles(t, root)
		if _, err := RebuildShadow(context.Background()); err != nil {
			t.Fatal(err)
		}
		seedRecallMeta(t, root)
		for _, query := range []string{"café", "naïve", "coöperate", "l'été", "snake_case", "agent-router", "abc123", "Ελληνικά", "中文", "русский", "cafe\u0301"} {
			if wantRecallParity(t, query, 0) == 0 {
				t.Fatalf("row path never served %q", query)
			}
		}
	}
}

func ageRecallFiles(t *testing.T, root string) {
	t.Helper()
	docs, err := shadowDocs(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * paths.RacyWindow)
	for _, doc := range docs {
		if err := os.Chtimes(filepath.Join(root, doc.path), aged, aged); err != nil {
			t.Fatal(err)
		}
	}
}
