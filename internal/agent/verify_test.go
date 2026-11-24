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
	var tb tailBuf
	for _, chunk := range []string{strings.Repeat("noise line\n", 400), "token ", secret[:20], secret[20:], strings.Repeat("\nnoise", 200)} {
		_, _ = tb.Write([]byte(chunk))
	}
	tail := verifyTail(tb.b)
	if strings.Contains(tail, secret[8:]) || strings.Contains(tail, secret[len(secret)-12:]) {
		t.Fatalf("tail leaks a secret shape or its suffix: %q", tail)
	}
	if !strings.Contains(tail, "REDACTED") {
		t.Fatalf("tail should hold the mask: %q", tail)
	}
}
