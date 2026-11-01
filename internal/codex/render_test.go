package codex

import (
	"reflect"
	"testing"
)

func TestSplitTopLevelInlineKeepsQuotedDelimiters(t *testing.T) {
	in := `{ name = "x", http_headers = { "anthropic-beta" = "a,b}{" }, lit = 'c,d' }`
	want := []string{
		`name = "x"`,
		`http_headers = { "anthropic-beta" = "a,b}{" }`,
		`lit = 'c,d'`,
	}
	if got := splitTopLevelInline(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSplitTopLevelInlineEscapedQuote(t *testing.T) {
	in := `{ a = "x\",y", b = 1 }`
	want := []string{`a = "x\",y"`, `b = 1`}
	if got := splitTopLevelInline(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
