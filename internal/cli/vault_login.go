package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// loginVerifyTimeout bounds the one token-endpoint call made after a login secret is written.
const loginVerifyTimeout = 90 * time.Second

// printLoginVerdict verifies the just-stored login of p once and prints the one-line result.
func printLoginVerdict(p *providers.Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), loginVerifyTimeout)
	defer cancel()
	fmt.Fprintln(os.Stderr, providers.VerifyStoredLogin(ctx, p))
}

// verifyIfLogin verifies name's login after it was stored; nothing happens for any other secret.
func verifyIfLogin(name string) {
	if p := providers.LoginSecretProvider(name); p != nil {
		printLoginVerdict(p)
	}
}

// loginPairsOnly drops each login pair whose value is no login record, warning by name; other pairs pass untouched.
func loginPairsOnly(pairs []vault.Pair) []vault.Pair {
	var ok []vault.Pair
	for _, p := range pairs {
		if providers.LoginSecretProvider(p.Name) != nil {
			if err := providers.CheckLoginRecord(p.Name, p.Value); err != nil {
				fmt.Fprintf(os.Stderr, "vault import: warning: %v; skipped\n", err)
				continue
			}
		}
		ok = append(ok, p)
	}
	return ok
}
