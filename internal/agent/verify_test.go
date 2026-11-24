package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

func TestRunVerifyRecordsExit(t *testing.T) {
	ok := runVerify(context.Background(), t.TempDir(), "go vet ./nonexistentpkg", os.Environ())
	if ok.Exit == 0 || ok.Tail == "" || ok.Command != "go vet ./nonexistentpkg" {
		t.Fatalf("failed step must record a non-zero exit and output: %+v", ok)
	}
	if bad := runVerify(context.Background(), "", "rm -rf x", nil); bad.Exit != -1 || bad.Reason == "" {
		t.Fatalf("a disallowed shape must not run: %+v", bad)
	}
}

func TestVerifyTailMasksAStraddlingSecretShape(t *testing.T) {
	secret := textutil.CredentialFamilies()[0] + strings.Repeat("aB3dE5gH7j", 4) // built at runtime: no literal
	opaque := strings.Repeat("Zq9Xw2Lm7K", 6)
	for _, after := range []int{300, 1900, 5000, 20000} { // the secrets land across the window trim and the final cut
		var tb tailBuf
		for _, chunk := range []string{strings.Repeat("noise\n", 2*verifyKeep/6-6), `{"h":"Bearer ` + opaque[:25], opaque[25:] + `"} token `,
			secret[:20], secret[20:], strings.Repeat("y", after)} {
			_, _ = tb.Write([]byte(chunk))
		}
		tail := verifyTail(&tb)
		for _, s := range []string{opaque, secret} {
			for i := 0; i+12 <= len(s); i++ {
				if strings.Contains(tail, s[i:i+12]) {
					t.Fatalf("after %d: tail leaks %q of a secret shape: %.80q", after, s[i:i+12], tail)
				}
			}
		}
	}
}

func TestVerifyTailDropsAnEndlessCutKeyBody(t *testing.T) {
	line := strings.Repeat("Zq9Xw2Lm7KaB3dE5", 4)
	tb := &tailBuf{b: []byte("mid-line " + line[:20] + "\n" + strings.Repeat(line+"\n", 5) + line[:5] + "\n"), cut: true}
	if tail := verifyTail(tb); tail != "" {
		t.Errorf("tail = %q, want empty for a window of base64 body lines", tail)
	}
	tb = &tailBuf{b: []byte("x\n" + line + "\n" + line + "\nok: build passed\n" + line + "\n"), cut: true}
	if tail := verifyTail(tb); !strings.HasPrefix(tail, "ok: build passed") {
		t.Errorf("tail = %q, want leading body lines dropped and ordinary output kept", tail)
	}
	tb = &tailBuf{b: []byte("partial\n" + line[:16] + "\n" + line[:5] + "\nFAIL pkg\n"), cut: true}
	if tail := verifyTail(tb); tail != "FAIL pkg\n" {
		t.Errorf("tail = %q, want short fragments after the cut dropped", tail)
	}
	tb = &tailBuf{b: []byte("x\n    " + line + "\r\n" + line + "  \r\n" + line[:7] + "\nFAIL pkg\n"), cut: true}
	if tail := verifyTail(tb); tail != "FAIL pkg\n" {
		t.Errorf("tail = %q, want indented, CRLF and padded body lines dropped", tail)
	}
	hash := strings.Repeat("0123456789abcdef", 4)
	tb = &tailBuf{b: []byte("x\n" + hash + "\nFAIL pkg\n"), cut: true}
	if tail := verifyTail(tb); tail != "FAIL pkg\n" {
		t.Errorf("tail = %q, want a hex line at the cut front dropped", tail)
	}
	tb = &tailBuf{b: []byte("x\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\n" + line + "\n" + line + "\nFAIL pkg\n"), cut: true}
	if tail := verifyTail(tb); tail != "FAIL pkg\n" {
		t.Errorf("tail = %q, want encrypted-PEM headers, blank line and body dropped", tail)
	}
}

func TestVerifyTailDropsAKeyBodyOpenedFarAboveTheCut(t *testing.T) {
	var tb tailBuf
	_, _ = tb.Write([]byte("-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("QUJD\n", 3*verifyKeep/5) + "plain text line\n"))
	if tail := verifyTail(&tb); strings.Contains(tail, "QUJD") || !tb.openPEM {
		t.Errorf("tail = %.60q, openPEM = %v, want the open key body hidden", tail, tb.openPEM)
	}
}

func TestVerifyTailDropsAPEMBodyWhoseBeginWasCut(t *testing.T) {
	body := strings.Repeat("Zq9Xw2Lm7KaB3dE5gH7jQ1wE4rT6yU8iO0pAsDfGhJk", 3)
	tb := &tailBuf{b: []byte("tail of one\n" + body + "\n" + body + "\n-----END PRIVATE KEY-----\nafter: ok\n"), cut: true}
	if tail := verifyTail(tb); strings.Contains(tail, body[:30]) || !strings.Contains(tail, "after") {
		t.Errorf("tail = %q, want the orphaned key body dropped and later output kept", tail)
	}
}
