package googletools

import (
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestParse(t *testing.T) {
	got, err := Parse([]string{"search", " url ", "search"})
	if err != nil || !slices.Equal(got, []Tool{Search, URL}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := Parse([]string{"computer"}); err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("computer: %v", err)
	}
	if _, err := Parse([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), "one of search,url,code") {
		t.Fatalf("bogus: %v", err)
	}
}
