package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/keyring"
)

// Check is one vault-health row; a failed Gate row fails doctor --secrets, the rest only advise.
type Check struct {
	Name   string
	OK     bool
	Gate   bool
	Detail string
}

// DoctorChecks checks vault permissions, master and retired keys, and credential names without reading values.
func DoctorChecks(reg *providers.Registry) []Check {
	var out []Check
	out = append(out, checkVaultPerms()...)
	out = append(out, checkMasterKey()...)
	out = append(out, checkRetiredKeys()...)
	out = append(out, checkKeyNames(reg)...)
	return append(out, checkKeyringTool())
}

// checkVaultPerms flags a vault.db readable/writable by anyone but its owner.
func checkVaultPerms() []Check {
	file := vault.Path()
	fi, err := os.Stat(file)
	row := Check{Name: "vault perms", OK: true, Gate: true}
	switch {
	case os.IsNotExist(err):
		row.Detail = "no vault yet (nothing to check)"
	case err != nil:
		row.OK, row.Detail = false, fmt.Sprintf("could not stat %s: %v", file, err)
	case fi.Mode().Perm()&0o077 != 0:
		row.OK, row.Detail = false, fmt.Sprintf("%s is %04o, want 0600 — run: chmod 600 %s", file, fi.Mode().Perm(), file)
	default:
		row.Detail = file + " is 0600"
	}
	return append([]Check{row}, checkArchivePerms()...)
}

// checkArchivePerms flags vault.db.rolledback-* archives and migration .bak files (from
// state/vault-migration.json) readable/writable by anyone but their owner.
func checkArchivePerms() []Check {
	var out []Check
	matches, _ := filepath.Glob(vault.Path() + ".rolledback-*")
	for _, m := range append(matches, manifestBackups()...) {
		if c, bad := flagPerm(m); bad {
			out = append(out, c)
		}
	}
	return out
}

// flagPerm reports a mode > 0600 file; a missing file is not a finding.
func flagPerm(file string) (Check, bool) {
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return Check{}, false
	}
	return Check{Name: "vault archive perms", Gate: true,
		Detail: fmt.Sprintf("%s is %04o, want 0600 — run: chmod 600 %s", file, fi.Mode().Perm(), file)}, true
}

// manifestBackups reads state/vault-migration.json directly (not internal/configure, which imports this
// package) for each item's "backup" path.
func manifestBackups() []string {
	raw, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "state", "vault-migration.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Items []struct {
			Backup string `json:"backup"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var out []string
	for _, item := range doc.Items {
		if item.Backup != "" {
			out = append(out, item.Backup)
		}
	}
	return out
}

// MasterKeyStatus resolves the vault's secret count and master-key source in one place, shared by the
// "vault master key" doctor row and launch's "vault" summary row so both describe the same check.
// count is 0 and err is non-nil when NamesErr itself failed (vault unreadable); count is 0 and err is
// nil when the vault holds no secrets (no master key needed); count > 0 and err non-nil when the
// resolved master key cannot open the vault.
func MasterKeyStatus() (count int, source string, err error) {
	names, err := NamesErr()
	if err != nil {
		return 0, "", err
	}
	count = len(names)
	if count == 0 {
		return 0, "not needed", nil
	}
	_, source, err = vault.MasterKey()
	return count, source, err
}

// checkMasterKey fails when the vault holds secrets the resolved master key cannot open, or when the
// vault itself can't be read (locked, wrong key) — an unreadable vault must not read as "no credential".
func checkMasterKey() []Check {
	row := Check{Name: "vault master key", OK: true, Gate: true}
	count, source, err := MasterKeyStatus()
	switch {
	case err != nil && count == 0:
		row.OK, row.Detail = false, "vault is unreadable: "+keyErrText(err)
		return []Check{row}
	case err != nil:
		row.OK, row.Detail = false, fmt.Sprintf("vault holds %d secret(s) but the master key does not open it: %s", count, keyErrText(err))
		return []Check{row}
	case count == 0:
		return nil
	}
	row.Detail = fmt.Sprintf("%d secret(s); master key from %s", count, source)
	return []Check{row}
}

// checkRetiredKeys reports whether archive keys remain available in the keyring (advisory).
func checkRetiredKeys() []Check {
	var id string
	var kids []string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		kids = vault.Retired(t)
		return nil
	}); err != nil {
		return []Check{{Name: "vault retired keys", Detail: err.Error()}}
	}
	var out []Check
	for _, kid := range kids {
		row := Check{Name: "vault retired key " + kid}
		_, found, err := keyring.Lookup("vault-key/" + id + "." + kid)
		switch {
		case err != nil:
			row.Detail = err.Error()
		case found:
			row.OK, row.Detail = true, "retained"
		default:
			row.Detail = "missing from the keyring"
		}
		out = append(out, row)
	}
	return out
}

// TailscaleNames are the Tailscale credentials the harness stores; not provider keys, so checkKeyNames knows them.
var TailscaleNames = []string{
	"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
}

// checkKeyNames warns about a stored key that matches no provider's auth.env — likely a typo, but
// not necessarily wrong (a provider not yet in the registry), so this never fails the gate.
func checkKeyNames(reg *providers.Registry) []Check {
	if reg == nil { // no registry to compare against: every key would read as a typo
		return nil
	}
	known := map[string]bool{providers.OAuthTokenEnv: true}
	for _, n := range TailscaleNames {
		known[n] = true
	}
	for _, p := range reg.Providers {
		if p.Auth.Env != "" {
			known[p.Auth.Env] = true
		}
	}
	names := Names()
	var typos []string
	for _, k := range names {
		if known[k] {
			continue
		}
		// Only a near miss of a known name is a likely typo; other names (JIRA_TOKEN, …) are the user's own.
		for n := range known {
			if editDistance(k, n) <= 2 {
				typos = append(typos, k+" (did you mean "+n+"?)")
				break
			}
		}
	}
	if len(typos) == 0 {
		return []Check{{Name: "vault key names", OK: true, Detail: fmt.Sprintf("%d name(s), no likely typos", len(names))}}
	}
	return []Check{{Name: "vault key names", Detail: strings.Join(typos, ", ")}}
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// checkKeyringTool is informational: without one the master key must come from env or key_command.
func checkKeyringTool() Check {
	if keyring.Available() {
		return Check{Name: "keyring tool", OK: true, Detail: keyring.Tool() + " on PATH"}
	}
	return Check{Name: "keyring tool", OK: true, Detail: "none on PATH — set HARNESS_VAULT_KEY or the vault key_command"}
}

// keyCmdStderr matches the key_command stderr tail vault appends to its error; it could echo key material.
var keyCmdStderr = regexp.MustCompile(`(key_command: [^:]*): .*`)

// keyErrText is err's text with any key_command stderr withheld, safe for doctor text and JSON rows.
func keyErrText(err error) string {
	return keyCmdStderr.ReplaceAllString(err.Error(), "$1 (stderr withheld)")
}
