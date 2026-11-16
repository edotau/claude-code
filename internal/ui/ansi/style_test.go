package ansi

import (
	"testing"
)

// TestStatusNotVerifiedStaysGray: the longer "not verified" token wins, so its "verified" is never painted green.
func TestStatusNotVerifiedStaysGray(t *testing.T) {
	got := Status(true, "gemini  not verified")
	if want := "gemini  " + Dim + "not verified" + Reset; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
	if got := Status(true, "verified"); got != Green+"verified"+Reset {
		t.Fatalf("Status(verified) = %q", got)
	}
}

// A token inside a longer word stays unpainted; edges that are not word characters ("FIX:") still match.
func TestStatusTokensMatchWholeWords(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"unpinned", "unpinned"},
		{"representation", "representation"},
		{"pinned: x", Green + "pinned" + Reset + ": x"},
		{"FIX: y", Red + "FIX:" + Reset + " y"},
		{"(present)", "(" + Green + "present" + Reset + ")"},
	} {
		if got := Status(true, c.in); got != c.want {
			t.Errorf("Status(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
