package redact

import "testing"

func TestHeadersAndURL(t *testing.T) {
	if got := URL("https://u:pw@h/p?a=1&api_key=zz"); got != "https://u:REDACTED@h/p?a=1&api_key=REDACTED" {
		t.Fatalf("URL = %q", got)
	}
}
