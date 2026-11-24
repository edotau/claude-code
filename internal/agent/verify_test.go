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
	for _, after := range []int{300, 1900, 4200, 9000} { // the secrets land across the trim and the final cut
		var tb tailBuf
		for _, chunk := range []string{strings.Repeat("x", 7000), `{"h":"Bearer ` + opaque[:25], opaque[25:] + `"} token `,
			secret[:20], secret[20:], strings.Repeat("y", after)} {
			_, _ = tb.Write([]byte(chunk))
		}
		tail := verifyTail(tb.b)
		for _, s := range []string{opaque, secret} {
			for i := 0; i+12 <= len(s); i++ {
				if strings.Contains(tail, s[i:i+12]) {
					t.Fatalf("after %d: tail leaks %q of a secret shape: %.80q", after, s[i:i+12], tail)
				}
			}
		}
	}
}
