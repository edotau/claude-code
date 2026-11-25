package memory

import (
	"testing"
	"time"
)

func TestDatedNameAndStripRepoPrefix(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"repo--2026-01-02-x.md": "2026-01-02-x.md",
		"20260102-x.md":         "2026-01-02-x.md",
		"x-2026-03-04.md":       "2026-03-04-x-2026-03-04.md",
		"plain.md":              "2026-11-25-plain.md",
	} {
		if got := DatedName(in, now); got != want {
			t.Errorf("DatedName(%q) = %q, want %q", in, got, want)
		}
	}
	if rest, key := StripRepoPrefix("a--b--c.md"); rest != "c.md" || key != "a" {
		t.Errorf("StripRepoPrefix = %q, %q", rest, key)
	}
	if rest, key := StripRepoPrefix("plain.md"); rest != "plain.md" || key != "" {
		t.Errorf("StripRepoPrefix plain = %q, %q", rest, key)
	}
}
