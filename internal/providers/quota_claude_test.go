package providers

import "testing"

// Claude's own limit messages are account quota; a fast-mode limit is not.
func TestIsQuotaClaudeMessages(t *testing.T) {
	for text, want := range map[string]bool{
		"You've hit your limit · resets 3pm":             true,
		"Out of usage credits":                           true,
		"You've hit your channel's monthly spend limit.": true,
		"You've hit your fast limit":                     false,
	} {
		if got := IsQuota(text); got != want {
			t.Errorf("IsQuota(%q) = %v, want %v", text, got, want)
		}
	}
}
