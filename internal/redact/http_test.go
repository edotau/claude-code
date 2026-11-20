package redact

import (
	"net/http"
	"testing"
)

func TestHeadersAndURL(t *testing.T) {
	h := Headers(http.Header{"Authorization": {"Bearer x"}, "X-Auth-Token": {"t"}, "Accept": {"a"}})
	if h.Get("Authorization") != HeaderMask || h.Get("X-Auth-Token") != HeaderMask || h.Get("Accept") != "a" {
		t.Fatalf("headers = %v", h)
	}
	if got := URL("https://u:pw@h/p?a=1&api_key=zz"); got != "https://u:REDACTED@h/p?a=1&api_key=REDACTED" {
		t.Fatalf("URL = %q", got)
	}
}
