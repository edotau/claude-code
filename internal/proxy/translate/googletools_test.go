package translate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseGoogleTools(t *testing.T) {
	got, err := ParseGoogleTools([]string{"search", " url ", "search"})
	if err != nil || !slices.Equal(got, []GoogleTool{GoogleSearch, GoogleURL}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := ParseGoogleTools([]string{"computer"}); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("computer: %v", err)
	}
	if _, err := ParseGoogleTools([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "one of search,url,code") {
		t.Fatalf("bogus: %v", err)
	}
	b, _ := json.Marshal(GoogleToolEntries([]GoogleTool{GoogleURL, GoogleCode}))
	if want := `[{"urlContext":{}},{"codeExecution":{}}]`; string(b) != want {
		t.Fatalf("entries %s, want %s", b, want)
	}
}
