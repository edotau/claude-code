package keyring

import "testing"

// hermetic points PATH at an empty dir so no real `security`/`secret-tool` is ever reachable.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	down = false
	t.Cleanup(func() { down = false })
}

// Get/Set/Delete on a box with no keyring tool on PATH must degrade, never panic or hang, and
// never touch the real OS keychain regardless of what runs this test.
func TestDegradesWithoutTool(t *testing.T) {
	hermetic(t)
	if Available() {
		t.Fatal("Available() must be false with PATH pointed at an empty dir")
	}
	if _, ok := Get("ANYTHING"); ok {
		t.Fatal("Get must miss when no keyring tool is available")
	}
	if err := Set("ANYTHING", "v"); err == nil {
		t.Fatal("Set must error when no keyring tool is available")
	}
	if err := Delete("ANYTHING"); err != nil {
		t.Fatalf("Delete must be a no-op, not an error, when no keyring tool is available: %v", err)
	}
}

func TestSetRejectsEmptyAndMultiline(t *testing.T) {
	hermetic(t)
	if err := Set("NAME", ""); err == nil {
		t.Fatal("Set must reject an empty value")
	}
	if err := Set("", "v"); err == nil {
		t.Fatal("Set must reject an empty name")
	}
	if err := Set("NAME", "line1\nline2"); err == nil {
		t.Fatal("Set must reject a value spanning more than one line")
	}
}

// quote must escape a backslash and a double quote so the `security -i` tokenizer sees one argument.
func TestQuoteEscapesBackslashAndDoubleQuote(t *testing.T) {
	got := quote(`a\b"c`)
	want := `"a\\b\"c"`
	if got != want {
		t.Errorf("quote(%q) = %q, want %q", `a\b"c`, got, want)
	}
}

func TestQuotePlain(t *testing.T) {
	if got := quote("plain"); got != `"plain"` {
		t.Errorf("quote(plain) = %q, want %q", got, `"plain"`)
	}
}
