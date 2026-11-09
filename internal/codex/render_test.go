package codex

import (
	"io"
	"reflect"
	"testing"
)

func TestCodexCredStoreKeyIgnoresMultilineInstructions(t *testing.T) {
	for _, quote := range []string{`"""`, `'''`} {
		t.Run(quote, func(t *testing.T) {
			existing := "instructions = " + quote + "\n  cli_auth_credentials_store = \"file\"\n" + quote + "\n"
			want := "cli_auth_credentials_store = \"keyring\"\n"
			if got := codexCredStoreKey(existing, io.Discard); got != want {
				t.Fatalf("multiline instructions suppressed the keyring setting: got %q, want %q", got, want)
			}
		})
	}
}

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
