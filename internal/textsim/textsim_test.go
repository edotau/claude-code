package textsim

import (
	"slices"
	"testing"
)

func TestTokenize(t *testing.T) {
	got := Tokenize("Run `make-test` NOW, café 42!")
	want := []string{"run", "make", "test", "now", "café", "42"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if len(Tokenize("  --- ")) != 0 {
		t.Error("punctuation-only input should yield no tokens")
	}
}
