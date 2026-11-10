package diff

import (
	"testing"
)

// Pins Python json.dumps(indent=2) bytes: ensure_ascii (incl. surrogate pairs), no HTML escaping.
func TestIndentJSONEnsureASCII(t *testing.T) {
	got, err := indentJSON(map[string]string{"k": "é<>&😀"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"k\": \"\\u00e9<>&\\ud83d\\ude00\"\n}"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
