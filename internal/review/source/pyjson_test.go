package source

import "testing"

// Pins Python json.dumps(indent=2) bytes: ensure_ascii (incl. surrogate pairs), no HTML escaping.
func TestPyMarshalIndentEnsureASCII(t *testing.T) {
	got, err := PyMarshalIndent(map[string]string{"k": "é<>&😀"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"k\": \"\\u00e9<>&\\ud83d\\ude00\"\n}"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPyEscapeNonASCII(t *testing.T) {
	in := "a" + string(rune(0xe9)) + string(rune(0x1F600))
	if got := string(PyEscapeNonASCII([]byte(in))); got != "a\\u00e9\\ud83d\\ude00" {
		t.Fatalf("got %q", got)
	}
}
