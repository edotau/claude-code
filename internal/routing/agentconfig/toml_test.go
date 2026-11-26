package agentconfig

import (
	"reflect"
	"testing"
)

func TestInsertTopLevelTOMLQuotedReplacementKey(t *testing.T) {
	for _, key := range []string{`"model"`, `'model'`} {
		for _, suffix := range []string{"", "[profile]\nmodel = \"table\"\n"} {
			t.Run(key+suffix, func(t *testing.T) {
				section := key + " = \"new\"\n"
				want := section
				if suffix != "" {
					want += "\n" + suffix
				}
				got := InsertTopLevelTOML("model = \"old\"\n"+suffix, section)
				if got != want {
					t.Fatalf("quoted replacement must replace the top-level key and preserve table keys: got %q, want %q", got, want)
				}
			})
		}
	}
}

func TestSplitInlineTableKeepsQuotedDelimiters(t *testing.T) {
	in := `{ name = "x", http_headers = { "anthropic-beta" = "a,b}{" }, lit = 'c,d' }`
	want := []string{
		`name = "x"`,
		`http_headers = { "anthropic-beta" = "a,b}{" }`,
		`lit = 'c,d'`,
	}
	if got := SplitInlineTable(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSplitInlineTableEscapedQuote(t *testing.T) {
	in := `{ a = "x\",y", b = 1 }`
	want := []string{`a = "x\",y"`, `b = 1`}
	if got := SplitInlineTable(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
