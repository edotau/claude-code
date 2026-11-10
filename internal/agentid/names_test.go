package agentid

import (
	"slices"
	"testing"
)

func TestAll(t *testing.T) {
	all := All()
	if want := []string{"claude", "codex", "copilot", "gemini", "opencode"}; !slices.Equal(all, want) {
		t.Fatalf("All() = %v, want %v", all, want)
	}
	for i, name := range all {
		if !Known(name) {
			t.Errorf("Known(%q) = false for a roster agent", name)
		}
		if slices.Contains(all[i+1:], name) {
			t.Errorf("All() lists %q twice", name)
		}
	}
	if Known("agy") || Known("") {
		t.Error("Known accepts a name outside the roster")
	}
	all[0], External()[0] = "mutated", "mutated"
	if All()[0] != Claude || External()[0] != Codex {
		t.Error("All or External shares state with a caller's slice")
	}
	if ext := External(); !slices.Equal(ext, All()[1:]) || slices.Contains(ext, Claude) {
		t.Errorf("External() = %v, want All() minus claude in order", ext)
	}
}

func TestBinary(t *testing.T) {
	for _, name := range All() {
		want := name
		if name == Gemini {
			want = "agy"
		}
		if got := Binary(name); got != want {
			t.Errorf("Binary(%q) = %q, want %q", name, got, want)
		}
	}
}
