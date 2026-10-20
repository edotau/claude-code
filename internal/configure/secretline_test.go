package configure

import (
	"bufio"
	"strings"
	"testing"
)

func TestSecretLineEchoesMaskedPreview(t *testing.T) {
	for _, in := range []string{"tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n",
		"TAILSCALE_OAUTH_CLIENT_SECRET=tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n"} {
		var out strings.Builder
		d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader(in))}
		if _, err := secretLine(d, "> "); err != nil {
			t.Fatal(err)
		}
		if w := len([]rune(strings.Split(strings.TrimSpace(out.String()), "\n")[1])); w > 80 {
			t.Errorf("preview line is %d columns", w)
		}
		if strings.Contains(out.String(), "0123456789") || !strings.Contains(out.String(), "(49 chars)") {
			t.Errorf("preview leaks or lacks length: %q", out.String())
		}
	}
}

func TestSecretLineFallbackPreservesInputAndOutput(t *testing.T) {
	for _, in := range []string{"", "\n", "  plain value  \r\n", "NAME=private-value\n", "unterminated-value"} {
		t.Run(strings.TrimSpace(in), func(t *testing.T) {
			d, out := newDialog(in)
			got, err := secretLine(d, "> ")
			want := strings.TrimRight(in, "\r\n")
			if err != nil || got != want {
				t.Fatalf("secretLine = (%q, %v), want (%q, nil)", got, err, want)
			}
			wantOut := "> \n"
			if _, value := ParseAssignment(want); value != "" {
				wantOut += "  pasted  " + pastePreview(want) + "\n"
			}
			if out.String() != wantOut {
				t.Errorf("output = %q, want %q", out.String(), wantOut)
			}
		})
	}
}
