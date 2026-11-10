package providers

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// Auto mode with no sealed user login goes straight to the paired SP: no user refresh is attempted.
func TestIdentityAutoWithoutUserLoginSkipsRefresh(t *testing.T) {
	p, m2m, refresh, _ := spHarness(t, "")
	if err := vault.Delete(AuthSecretName(p)); err != nil {
		t.Fatal(err)
	}
	got, err := Credential(context.Background(), p)
	if err != nil || got != "sp-token" || *m2m != 1 || *refresh != 0 {
		t.Fatalf("Credential = %q, %v (m2m %d, refresh %d)", got, err, *m2m, *refresh)
	}
	lock := filepath.Join(paths.StateDir(), "locks", "auth-"+lockNameRE.ReplaceAllString(p.Name, "_")+".lock")
	if _, err := os.Stat(lock); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the user refresh lock was taken: %v", err)
	}
}

// A sealed SP token is served by a new process without a mint and without the fallback note; a mint notes once.
func TestSPNoteOnlyOnMint(t *testing.T) {
	p, m2m, _, out := spHarness(t, IdentitySP)
	tok := jwtExpIn(time.Hour, "")
	mintM2M = func(context.Context, string, string, string) (oauth.Token, error) {
		*m2m++
		return oauth.Token{Access: tok}, nil
	}
	var mu sync.Mutex
	var notes []string
	ctx := WithNotes(context.Background(), func(s string) { mu.Lock(); notes = append(notes, s); mu.Unlock() })
	if v, err := Credential(ctx, p); err != nil || v != tok {
		t.Fatalf("first Credential = %v", err)
	}
	spCache.Delete(p.Name) // a new process: only the sealed _ACCESS slot remains
	if v, err := Credential(ctx, p); err != nil || v != tok {
		t.Fatalf("second Credential = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if *m2m != 1 || len(notes) != 1 || !strings.Contains(notes[0], "using the service principal sp-client") || out.Len() != 0 {
		t.Fatalf("m2m = %d, notes = %q, stderr note = %q", *m2m, notes, out.String())
	}
}
