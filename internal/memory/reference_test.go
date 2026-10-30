package memory

import (
	"strings"
	"testing"
)

// embeddedVerbs are every verb Go actually calls memory.Reference with — grepped by hand from
// update.go, dream.go, promote.go — each must resolve to a non-empty embedded prompt.
var embeddedVerbs = []string{"init", "review", "promote", "update", "dream"}

// TestReferenceEmbeddedVerbsNonEmpty: every verb Go calls Reference(verb) with resolves to a
// non-empty embedded prompt — no disk path left to go missing at runtime.
func TestReferenceEmbeddedVerbsNonEmpty(t *testing.T) {
	for _, verb := range embeddedVerbs {
		got, err := Reference(verb)
		if err != nil {
			t.Errorf("Reference(%q): %v", verb, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Reference(%q) = empty, want a non-empty procedure", verb)
		}
	}
}

// TestReferenceUnknownVerbErrors: a verb with no embedded prompts/<verb>.md is an error, not an
// empty string — a caller must never run a background job on a silently empty prompt.
func TestReferenceUnknownVerbErrors(t *testing.T) {
	if _, err := Reference("does-not-exist"); err == nil {
		t.Fatal("Reference(\"does-not-exist\") = nil error, want one")
	}
}

// TestReferenceEmptyVerbErrors: the empty string is also an unknown verb.
func TestReferenceEmptyVerbErrors(t *testing.T) {
	if _, err := Reference(""); err == nil {
		t.Fatal("Reference(\"\") = nil error, want one")
	}
}
