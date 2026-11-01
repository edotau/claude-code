package cli

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestTakeAsBeatsProfileIdentity(t *testing.T) {
	as, rest, err := takeAs([]string{"--model", "x", "--as", "sp", "--", "--as", "user"})
	if err != nil || as != providers.IdentitySP {
		t.Fatalf("as=%q err=%v", as, err)
	}
	if len(rest) != 5 || rest[3] != "--as" {
		t.Fatalf("rest=%v", rest)
	}
	// the flag wins over a profile set to user
	if got, _ := providers.ResolveIdentity(providers.IdentityUser, as); got != providers.IdentitySP {
		t.Fatalf("resolved %q", got)
	}
	if _, _, err := takeAs([]string{"--as=bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	if err := askAs("sp", "codex"); err == nil {
		t.Fatal("--as accepted for codex")
	}
}
