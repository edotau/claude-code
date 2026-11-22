package rtkwrap

import (
	"testing"

	"github.com/edotau/claude-code/internal/shell"
)

// TestNamesAreRTKWords keeps every shim a command rtk rewrites, so a shim never wraps what rtk cannot compress.
func TestNamesAreRTKWords(t *testing.T) {
	words := shell.GuardSet("rtk_rewrite_words")
	for _, n := range Names {
		if !words[n] {
			t.Errorf("%s is shimmed but not in guards.json rtk_rewrite_words", n)
		}
	}
}
