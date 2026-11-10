package oauth

import "testing"

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.cloud.databricks.com":            "https://example.cloud.databricks.com",
		" https://Example.cloud.databricks.com/ ": "https://example.cloud.databricks.com",
		"https://h.example.com:8443":              "https://h.example.com:8443",
		"http://h.example.com":                    "",
		"https://h.example.com/serving":           "",
		"https://h.example.com/?o=1":              "",
		"https://user@h.example.com":              "",
		"":                                        "",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
