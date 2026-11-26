package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"

	_ "github.com/edotau/claude-code/internal/nonet" // tests refuse real DNS: mock API calls
)

func TestRedact(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"clean unchanged", "plain  text\twith\nspace", "plain  text\twith\nspace"},
		{"empty", "", ""},
		{"non-secret hyphenated text unchanged", "my-feature_flag is-enabled for_user", "my-feature_flag is-enabled for_user"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RedactSecrets(c.in); got != c.want {
				t.Errorf("RedactSecrets(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"collapses whitespace", "a  b\n\tc   d", 100, "a b c d"},
		{"short input unchanged", "hello world", 11, "hello world"},
		{"n above length unchanged", "hello", 50, "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PreviewRedacted(c.in, c.n); got != c.want {
				t.Errorf("PreviewRedacted(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestPreviewTruncatesOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than n; When truncated; Then the result is valid UTF-8 and ends in "…"
	got := PreviewRedacted(strings.Repeat("😀", 30), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("PreviewRedacted split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 10 {
		t.Errorf("PreviewRedacted = %q (%d runes), want 10 runes ending in …", got, utf8.RuneCountInString(got))
	}
}

// Ordinary text must not trigger the shared credential detector.
func TestCredentialRERejectsOrdinaryText(t *testing.T) {
	cases := []struct {
		label, in string
		want      bool
	}{
		{"git sha", "01234567", false},
		{"uuid", "123e4567-e89b-12d3-a456-426614174000", false},
		{"base64 chunk", "Zm9v", false},
		{"kebab prose", "change-appearance-before-asking", false},
		{"certificate", "-----BEGIN CERTIFICATE-----", false},
		{"password assignment", "password=hunter2", false},
		{"databricks host", "adb-1234567890123456.7.azuredatabricks.net", false},
		{"model id", "databricks-claude-sonnet-5-5", false},
		{"kebab job id", "job-12345678901234567890", false},
	}
	for _, c := range cases {
		if got := CredentialRE().MatchString("note: " + c.in + " here"); got != c.want {
			t.Errorf("%s: flagged = %v, want %v", c.label, got, c.want)
		}
	}
}

func BenchmarkRedactSecrets(b *testing.B) {
	s := strings.Repeat("ordinary text with whitespace and punctuation! ", 50)
	for b.Loop() {
		RedactSecrets(s)
	}
}

// Ordinary input, including invalid UTF-8, has no credential spans.
func TestSecretSpansRejectsOrdinaryText(t *testing.T) {
	for _, input := range []string{"", "plain text", "two\nlines", "punctuation: [value]", "\xffsuffix"} {
		if spans := SecretSpans(input); len(spans) != 0 {
			t.Errorf("ordinary input has %d secret spans", len(spans))
		}
		if got := RedactSecrets(input); got != input {
			t.Error("ordinary input changed")
		}
	}
}

// Malformed values exercise missing-prefix and whitespace errors without vendor-shaped fixtures.
var validCredentialCases = []struct {
	name    string
	envName string
	value   string
	wantErr bool
}{
	{"unknown name always valid", "TAILSCALE_SOMETHING_ELSE", "anything at all", false},

	{"client id too short", "TAILSCALE_OAUTH_CLIENT_ID", "kshort1", true},
	{"client id too long", "TAILSCALE_OAUTH_CLIENT_ID", "k" + strings.Repeat("!", 40), true},
	{"client id not starting with k", "TAILSCALE_OAUTH_CLIENT_ID", "qzplmoknijbuhv", true},
	{"client id non-alnum", "TAILSCALE_OAUTH_CLIENT_ID", "k!plmoknijbuhv", true},
	{"client id whitespace", "TAILSCALE_OAUTH_CLIENT_ID", "k !", true},
	// real failure seen live: a full pbpaste'd shell pipeline, not a key.
	{"client id real failure: pasted shell pipeline", "TAILSCALE_OAUTH_CLIENT_ID",
		"! pbpaste | claude-code vault set TAILSCALE_OAUTH_CLIENT_ID", true},

	{"client secret wrong prefix", "TAILSCALE_OAUTH_CLIENT_SECRET", "not-a-secret", true},
	{"client secret whitespace", "TAILSCALE_OAUTH_CLIENT_SECRET", "qz !malformed", true},
	// A malformed pasted value is refused.
	{"client secret malformed paste", "TAILSCALE_OAUTH_CLIENT_SECRET",
		"qz!wrong-kind", true},

	{"api key wrong prefix", "TAILSCALE_API_KEY", "qz!malformed", true},
	{"api key whitespace", "TAILSCALE_API_KEY", "qz !malformed", true},

	{"authkey wrong prefix", "TAILSCALE_AUTHKEY", "qz!malformed", true},
	{"authkey whitespace", "TAILSCALE_AUTHKEY", "qz\t!malformed", true},
}

// Invalid credentials are refused without echoing the supplied value.
func TestValidCredential(t *testing.T) {
	for _, c := range validCredentialCases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidCredential(c.envName, c.value)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidCredential(%q, %q) = %v, wantErr %v", c.envName, c.value, err, c.wantErr)
			}
			if err == nil {
				return
			}
			if c.value != "" && strings.Contains(err.Error(), c.value) {
				t.Errorf("error echoes malformed input")
			}
		})
	}
}

// The error text never contains the full malformed value.
func TestValidCredentialNeverEchoesFullValue(t *testing.T) {
	cases := []struct {
		name, value string
	}{
		{"TAILSCALE_OAUTH_CLIENT_SECRET", "qz!wrong-kind"},
		{"TAILSCALE_OAUTH_CLIENT_ID", "! pbpaste | claude-code vault set TAILSCALE_OAUTH_CLIENT_ID"},
	}
	for _, c := range cases {
		err := ValidCredential(c.name, c.value)
		if err == nil {
			t.Fatalf("ValidCredential(%q, %q) = nil, want an error", c.name, c.value)
		}
		if c.value != "" && strings.Contains(err.Error(), c.value) {
			t.Errorf("%s: error %q echoes the full value", c.name, err.Error())
		}
	}
}

func TestContainsAny(t *testing.T) {
	if !ContainsAny("read-only file system", "denied", "only") || ContainsAny("abc", "x", "y") || ContainsAny("abc") {
		t.Fatal("ContainsAny wrong")
	}
}
