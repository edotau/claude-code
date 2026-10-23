package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	data, err := os.ReadFile(filepath.Join(Dir(root), "decisionLog.md"))
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
	// Round-trip: a second, later entry must still land ABOVE the first (newest-first).
	id2, err := Remember(root, "project", "a second, later decision", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(Dir(root), "decisionLog.md"))
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
			data, err := os.ReadFile(filepath.Join(Dir(root), "conventions.md"))
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

// TestRememberRoundTripsThroughSearch: a remembered fact is findable by SearchBanks on its own words.
func TestRememberRoundTripsThroughSearch(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id, err := Remember(root, "feedback", "always quote shell paths with spaces in scripts", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hits := SearchBanks(root, "quote shell paths spaces scripts", 5)
	found := false
	for _, h := range hits {
		if h.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("Remember's id %s not found by SearchBanks: hits=%+v", id, hits)
	}
}
