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
