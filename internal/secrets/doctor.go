package secrets

import (
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Doctor checks vault permissions, master and retired keys, and credential names without printing values.
func Doctor(out io.Writer, reg *providers.Registry) int {
	fmt.Fprintln(out, "== secrets doctor ==")
	fail := false
	fail = checkVaultPerms(out) || fail
	fail = checkMasterKey(out) || fail
	checkRetiredKeys(out)   // advisory only
	checkKeyNames(out, reg) // advisory only, never fails the gate
	checkKeyringTool(out)   // informational only

	if fail {
		fmt.Fprintln(out, "secrets doctor: problem(s) found.")
		return ExitFindings
	}
	fmt.Fprintln(out, "secrets doctor: clean.")
	return ExitClean
}

// checkVaultPerms flags a vault.db readable/writable by anyone but its owner.
func checkVaultPerms(out io.Writer) bool {
	file := vault.Path()
	fi, err := os.Stat(file)
	if os.IsNotExist(err) {
		fmt.Fprintln(out, "  ✓ no vault yet (nothing to check)")
		return false
	}
	if err != nil {
		fmt.Fprintf(out, "  ⚠ could not stat %s: %v\n", file, err)
		return true
	}
	if fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(out, "  ⚠ %s is %04o, want 0600 — run: chmod 600 %s\n", file, fi.Mode().Perm(), file)
		return true
	}
	fmt.Fprintln(out, "  ✓ "+file+" is 0600")
	return false
}

// checkMasterKey fails when the vault holds secrets the resolved master key cannot open.
func checkMasterKey(out io.Writer) bool {
	n := len(Names())
	if n == 0 {
		return false
	}
	_, source, err := vault.MasterKey()
	if err != nil {
		fmt.Fprintf(out, "  ⚠ vault holds %d secret(s) but the master key does not open it: %v\n", n, err)
		return true
	}
	fmt.Fprintf(out, "  ✓ %d secret(s); master key from %s\n", n, source)
	return false
}

// checkRetiredKeys reports whether archive keys remain available in the keyring.
func checkRetiredKeys(out io.Writer) {
	var id string
	var kids []string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		kids = vault.Retired(t)
		return nil
	}); err != nil {
		fmt.Fprintf(out, "  ⚠ retired keys: %v\n", err)
		return
	}
	for _, kid := range kids {
		_, found, err := keyring.Lookup("vault-key/" + id + "." + kid)
		switch {
		case err != nil:
			fmt.Fprintf(out, "  ⚠ retired key %s: %v\n", kid, err)
		case found:
			fmt.Fprintf(out, "  ✓ retired key %s retained\n", kid)
		default:
			fmt.Fprintf(out, "  ⚠ retired key %s missing from the keyring\n", kid)
		}
	}
}

// TailscaleNames are the Tailscale credentials the harness stores; not provider keys, so checkKeyNames knows them.
var TailscaleNames = []string{
	"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
}

// checkKeyNames warns about a stored key that matches no provider's auth.env — likely a typo, but
// not necessarily wrong (a provider not yet in the registry), so this never fails the gate.
func checkKeyNames(out io.Writer, reg *providers.Registry) {
	known := map[string]bool{providers.OAuthTokenEnv: true}
	for _, n := range TailscaleNames {
		known[n] = true
	}
	for _, p := range reg.Providers {
		if p.Auth.Env != "" {
			known[p.Auth.Env] = true
		}
	}
	for _, k := range Names() {
		if !known[k] {
			fmt.Fprintf(out, "  ⚠ %s matches no provider's auth.env in the registry — typo?\n", k)
		}
	}
}

// checkKeyringTool is informational: without one the master key must come from env or key_command.
func checkKeyringTool(out io.Writer) {
	if keyring.Available() {
		fmt.Fprintln(out, "  ✓ a keyring tool is on PATH ("+keyring.Tool()+")")
		return
	}
	fmt.Fprintln(out, "  ℹ no keyring tool on PATH — set HARNESS_VAULT_KEY or the vault key_command")
}
