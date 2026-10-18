package providers

import "testing"

// TestDisplayName: a catalog without names still gets a readable Model column.
func TestDisplayName(t *testing.T) {
	for id, want := range map[string]string{"models/gemini-2.5-flash": "Gemini 2.5 Flash", "gpt-6-astra": "Gpt 6 Astra"} {
		if got := displayName(id); got != want {
			t.Errorf("displayName(%q) = %q, want %q", id, got, want)
		}
	}
}
