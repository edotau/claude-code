package memory

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --load cuts an over-budget block at a rune boundary within recallLoadCap and loads nothing once the budget is spent.
func TestRecallLoadTruncatesOnRuneBoundary(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id, err := Remember(root, "feedback", strings.Repeat("—", recallLoadCap), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, block, err := ShowBlock(id)
	if err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, []SearchHit{{ID: id}, {ID: id}}, 2)
	loaded := strings.Split(out.String(), "\n<!-- "+id+" -->\n")
	if len(loaded) < 2 || errw.Len() != 0 {
		t.Fatalf("want loaded blocks and no errors, got stdout %q stderr %q", out.String(), errw.String())
	}
	body := strings.TrimSuffix(loaded[1], "\n")
	room := recallLoadCap - len("\n<!-- "+id+" -->\n\n") // the cap counts each block's id wrapper too
	if out.Len() > recallLoadCap || !strings.HasPrefix(strings.TrimSpace(block), body) || !utf8.ValidString(body) ||
		len(body) <= room-utf8.UTFMax {
		t.Errorf("loaded %d bytes, wrote %d, want a valid-UTF-8 block prefix cut just under %d", len(body), out.Len(), room)
	}
}

// TestRecallLoadStopsAfterCut: a rune-boundary cut leaves 1-2 bytes of budget, which must not print the next hit.
func TestRecallLoadStopsAfterCut(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	long, err := Remember(root, "feedback", "x"+strings.Repeat("—", recallLoadCap), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	short, err := Remember(root, "feedback", "short follow-up fact", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, block, err := ShowBlock(long)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneStart(strings.TrimSpace(block)[recallLoadCap]) {
		t.Fatal("fixture: the cap must fall inside a rune so the cut leaves budget over")
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, []SearchHit{{ID: long}, {ID: short}}, 2)
	if strings.Contains(out.String(), "<!-- "+short+" -->") || errw.Len() != 0 {
		t.Errorf("a hit after the cut block was loaded as a fragment:\n%q\nstderr %q", out.String()[out.Len()-60:], errw.String())
	}
}

// TestRecallLoadSkipsWhatDoesNotFit: a later block too big for the rest is skipped whole, a smaller one after it still
// loads, and the id wrappers count toward recallLoadCap.
func TestRecallLoadSkipsWhatDoesNotFit(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	var hits []SearchHit
	for _, fact := range []string{strings.Repeat("m", 3000), strings.Repeat("b", 2000), "small fact"} {
		id, err := Remember(root, "feedback", fact, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, SearchHit{ID: id})
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, hits, len(hits))
	if got := out.String(); strings.Contains(got, hits[1].ID) || !strings.Contains(got, "small fact") {
		t.Errorf("want the 2000-byte block skipped and the small one loaded after it, got %d bytes:\n%.200s", len(got), got)
	}
	var even []SearchHit // five blocks whose text fits the cap but whose wrappers do not
	for i := range 5 {
		id, err := Remember(root, "feedback", strings.Repeat(string(rune('c'+i)), 780), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		even = append(even, SearchHit{ID: id})
	}
	out.Reset()
	if recallLoad(&out, &errw, even, len(even)); out.Len() > recallLoadCap || errw.Len() != 0 {
		t.Errorf("wrote %d bytes, want at most %d; stderr %q", out.Len(), recallLoadCap, errw.String())
	}
}
