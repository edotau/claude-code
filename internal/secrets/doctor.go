package secrets

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Doctor runs the offline coherence checks: vault perms, whether the master key opens the stored
// secrets, key names against the provider registry, legacy env.d files, and the keyring tool.
// Never prints a value.
func Doctor(out io.Writer, reg *providers.Registry) int {
	fmt.Fprintln(out, "== secrets doctor ==")
	fail := false
	fail = checkVaultPerms(out) || fail
	fail = checkMasterKey(out) || fail
	checkLegacy(out)        // informational only
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

// checkLegacy notes pre-vault secret files that `config migrate` would import.
func checkLegacy(out io.Writer) {
	for _, name := range []string{"secrets.env", "secrets.meta.json"} {
		p := filepath.Join(paths.EnvDir(), name)
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(out, "  ℹ legacy %s found; `claude-code config migrate` imports it\n", p)
		}
	}
}

// checkKeyNames warns about a stored key that matches no provider's auth.env — likely a typo, but
// not necessarily wrong (a provider not yet in the registry), so this never fails the gate.
func checkKeyNames(out io.Writer, reg *providers.Registry) {
	known := map[string]bool{}
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
