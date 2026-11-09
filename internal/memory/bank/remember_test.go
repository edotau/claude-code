package bank

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
)

// TestRememberUnknownTypeErrors: an unmapped --type is refused, naming the valid set.
func TestRememberUnknownTypeErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Remember(root, "bogus", "a fact", time.Now()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Remember with an unknown type = %v, want an error naming the valid types", err)
	}
}

// TestRememberEmptyFactErrors: whitespace-only input is refused before any write.
func TestRememberEmptyFactErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Remember(root, "feedback", "   ", time.Now()); err == nil {
		t.Fatal("Remember with an empty fact = nil error, want one")
	}
}

func TestRememberFreshRepoKeepsItsStore(t *testing.T) {
	tempConfig(t)
	root := gitRepo(t, canonTemp(t, t.TempDir()), "fresh")
	dir, err := memstore.RepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Remember(root, "feedback", "keep fresh repository facts discoverable", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := memstore.Dir(root); got != dir {
		t.Fatalf("Remember orphaned its store: directory changed from %s to %s", dir, got)
	}
	origin, err := memstore.ReadOrigin(dir)
	if err != nil || !origin.Accepts(root) {
		t.Fatalf("Remember did not record the repository origin: %+v, %v", origin, err)
	}
	hits, err := CorpusBlocks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if BlockID(hit.Block) == id {
			return
		}
	}
	t.Fatalf("remembered id %s is absent from the repository corpus", id)
}

// TestRememberProjectTypeWritesDatedDecisionLogEntryAtTop: a "project" fact lands as a dated,
// newest-first heading in decisionLog.md, and the returned id round-trips through recall.
func TestRememberProjectTypeWritesDatedDecisionLogEntryAtTop(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	id, err := Remember(root, "project", "adopt bbolt for the memory store", now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned an empty id")
	}
	data, err := os.ReadFile(filepath.Join(memstore.Dir(root), "decisionLog.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "## 2026-10-23 — adopt bbolt for the memory store") {
		t.Errorf("decisionLog.md missing the dated heading: %q", body)
	}
	if !strings.Contains(body, "<!-- id:"+id+" -->") {
		t.Errorf("decisionLog.md missing the returned id %s: %q", id, body)
	}
	assertLaterProjectEntryFirst(t, root, id, now)
}

func assertLaterProjectEntryFirst(t *testing.T, root, id string, now time.Time) {
	t.Helper()
	// Round-trip: a second, later entry must still land ABOVE the first (newest-first).
	id2, err := Remember(root, "project", "a second, later decision", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(memstore.Dir(root), "decisionLog.md"))
	first := strings.Index(string(data), id2)
	second := strings.Index(string(data), id)
	if first == -1 || second == -1 || first > second {
		t.Errorf("newer entry (%s) did not land above the older one (%s): %q", id2, id, data)
	}
}

// TestRememberNonProjectTypesWriteConventionsBullet: user/feedback/reference all land as a labeled
// bullet in conventions.md, each carrying a persisted id.
func TestRememberNonProjectTypesWriteConventionsBullet(t *testing.T) {
	for _, kind := range []string{"user", "feedback", "reference"} {
		t.Run(kind, func(t *testing.T) {
			tempConfig(t)
			root := repoRoot(t, "proj")
			id, err := Remember(root, kind, "prefer early returns over deep nesting", time.Now())
			if err != nil {
				t.Fatalf("Remember(%s): %v", kind, err)
			}
			data, err := os.ReadFile(filepath.Join(memstore.Dir(root), "conventions.md"))
			if err != nil {
				t.Fatal(err)
			}
			label := strings.ToUpper(kind[:1]) + kind[1:]
			body := string(data)
			if !strings.Contains(body, "- "+label+": prefer early returns over deep nesting") {
				t.Errorf("conventions.md missing the labeled bullet: %q", body)
			}
			if !strings.Contains(body, "<!-- id:"+id+" -->") {
				t.Errorf("conventions.md missing the returned id %s: %q", id, body)
			}
		})
	}
}

// TestPrependBlockUnreadableLogIsNotRewritten: a read error other than not-exist leaves decisionLog.md alone.
func TestPrependBlockUnreadableLogIsNotRewritten(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "decisionLog.md") // a directory: ReadFile fails with EISDIR
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prependBlock(p, "decisionLog", "## 2026-11-01 — x"); err == nil {
		t.Fatal("prependBlock on an unreadable log = nil error, want the read error")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("unreadable log was replaced: %v", err)
	}
}

// TestLastStampedIDExactMatchAndDirection: a longer older bullet sharing the prefix is skipped, a multi-line
// fact matches on its first line, and an appended file resolves to the last match.
func TestLastStampedIDExactMatchAndDirection(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "conventions.md")
	body := "# c\n\n- User: foo bar <!-- id:aaaa1111 -->\n- User: foo <!-- id:bbbb2222 -->\n- User: foo <!-- id:cccc3333 -->\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := lastStampedID(p, "- User: foo", false); err != nil || got != "cccc3333" {
		t.Fatalf("appended file: got %q, %v, want cccc3333", got, err)
	}
	if got, err := lastStampedID(p, "- User: foo", true); err != nil || got != "bbbb2222" {
		t.Fatalf("newest-first file: got %q, %v, want bbbb2222", got, err)
	}
	if got, err := lastStampedID(p, "- User: foo bar\ncontinued", false); err != nil || got != "aaaa1111" {
		t.Fatalf("multi-line fact: got %q, %v, want aaaa1111", got, err)
	}
}
