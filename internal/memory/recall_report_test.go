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
	total := len(strings.Join(loaded[1:], "")) - (len(loaded) - 1) // each block is printed with one trailing newline
	if total > recallLoadCap || !strings.HasPrefix(strings.TrimSpace(block), body) || !utf8.ValidString(body) ||
		len(body) <= recallLoadCap-utf8.UTFMax {
		t.Errorf("loaded %d of %d bytes total, want a valid-UTF-8 block prefix cut just under %d", len(body), total, recallLoadCap)
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
