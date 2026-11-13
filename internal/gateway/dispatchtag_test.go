package gateway

import "testing"

// DispatchBase tags a base with a valid id and leaves off one the proxy would 400.
func TestDispatchBase(t *testing.T) {
	const base = "http://127.0.0.1:1"
	for id, want := range map[string]string{
		"20261113T144909.123-codex": base + "/d/20261113T144909.123-codex",
		"":                          base,
		"a_b":                       base,
		"a/b":                       base,
	} {
		if got := DispatchBase(base, id); got != want {
			t.Errorf("DispatchBase(%q) = %q, want %q", id, got, want)
		}
	}
}
