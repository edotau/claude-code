package github

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A /user body past the 64 KiB cap reads as a failed lookup, never a login decoded from an unbounded read.
func TestFetchGitHubLoginCapsBody(t *testing.T) {
	big := `{"login":"` + strings.Repeat("a", 128<<10) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("big") != "" {
			_, _ = io.WriteString(w, big)
			return
		}
		_, _ = io.WriteString(w, `{"login":"octocat"}`)
	}))
	t.Cleanup(srv.Close)
	orig := gitHubUserURL
	t.Cleanup(func() { gitHubUserURL = orig })
	gitHubUserURL = srv.URL
	if got := fetchGitHubLogin("fake-token"); got != "octocat" {
		t.Fatalf("small body login = %q, want octocat", got)
	}
	gitHubUserURL = srv.URL + "?big=1"
	if got := fetchGitHubLogin("fake-token"); got != "" {
		t.Fatalf("oversized body gave a %d-byte login, want \"\" (body uncapped)", len(got))
	}
}
