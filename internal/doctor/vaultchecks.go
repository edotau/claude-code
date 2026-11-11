package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// vaultCheck is one vault-health row; a failed Gate row fails doctor --secrets, the rest only advise.
type vaultCheck struct {
	Name   string
	OK     bool
	Gate   bool
	Detail string
}

// vaultChecks checks vault permissions, master and retired keys, and credential names without reading values.
func vaultChecks(reg *providers.Registry) []vaultCheck {
	var out []vaultCheck
	out = append(out, checkVaultPerms()...)
	out = append(out, checkMasterKey()...)
	out = append(out, checkRetiredKeys()...)
	out = append(out, checkKeyNames(reg)...)
	return append(out, checkKeyringTool())
}

// permsGOOS decides whether mode bits mean anything; a seam so tests run the Windows branch on any host.
var permsGOOS = runtime.GOOS

// loosePerm reports a file open beyond its owner; Windows has no such bits (Stat reports 0666), as vault's looseMode.
func loosePerm(fi os.FileInfo) bool { return permsGOOS != "windows" && fi.Mode().Perm()&0o077 != 0 }

// checkVaultPerms flags a vault.db readable/writable by anyone but its owner.
func checkVaultPerms() []vaultCheck {
	file := vault.Path()
	fi, err := os.Stat(file)
	row := vaultCheck{Name: "vault perms", OK: true, Gate: true}
	switch {
	case os.IsNotExist(err):
		row.Detail = "no vault yet (nothing to check)"
	case err != nil:
		row.OK, row.Detail = false, fmt.Sprintf("could not stat %s: %v", file, err)
	case loosePerm(fi):
		row.OK, row.Detail = false, fmt.Sprintf("%s is %04o, want 0600 — run: chmod 600 %s", file, fi.Mode().Perm(), file)
	default:
		row.Detail = file + " is 0600"
	}
	return append([]vaultCheck{row}, checkArchivePerms()...)
}

// checkArchivePerms flags vault.db.rolledback-* archives and migration .bak files (from
// state/vault-migration.json) that are readable beyond the owner; an unreadable listing or manifest is a failing row.
func checkArchivePerms() []vaultCheck {
	var out []vaultCheck
	matches, err := rolledBackArchives()
	if err != nil {
		out = append(out, vaultCheck{Name: "vault archive perms", Gate: true,
			Detail: "could not list the vault directory, so its rolledback archives were not checked: " + err.Error()})
	}
	backups, err := manifestBackups()
	if err != nil {
		out = append(out, vaultCheck{Name: "vault archive perms", Gate: true,
			Detail: "could not read the migration manifest, so its .bak files were not checked: " + err.Error()})
	}
	for _, m := range append(matches, backups...) {
		if c, bad := flagPerm(m); bad {
			out = append(out, c)
		}
	}
	return out
}

// rolledBackArchives lists vault.db.rolledback-* beside the vault; a missing directory holds none.
func rolledBackArchives() ([]string, error) {
	file := vault.Path()
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(file) + ".rolledback-"
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// flagPerm reports a mode > 0600 file, or one that cannot be inspected; a missing file is not a finding.
func flagPerm(file string) (vaultCheck, bool) {
	fi, err := os.Stat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return vaultCheck{}, false
	}
	if err != nil {
		return vaultCheck{Name: "vault archive perms", Gate: true, Detail: fmt.Sprintf("could not stat %s: %v", file, err)}, true
	}
	if !loosePerm(fi) {
		return vaultCheck{}, false
	}
	return vaultCheck{Name: "vault archive perms", Gate: true,
		Detail: fmt.Sprintf("%s is %04o, want 0600 — run: chmod 600 %s", file, fi.Mode().Perm(), file)}, true
}

// manifestBackups reads state/vault-migration.json directly (not internal/configure, which imports this
// package) for each item's "backup" path; an absent manifest is benign, an unreadable or corrupt one is an error.
func manifestBackups() ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "state", "vault-migration.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Items []struct {
			Backup string `json:"backup"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	var out []string
	for _, item := range doc.Items {
		if item.Backup != "" {
			out = append(out, item.Backup)
		}
	}
	return out, nil
}

// checkMasterKey fails when the vault holds secrets the resolved master key cannot open, or when the
// vault itself can't be read (locked, wrong key) — an unreadable vault must not read as "no credential".
func checkMasterKey() []vaultCheck {
	row := vaultCheck{Name: "vault master key", OK: true, Gate: true}
	count, source, err := vault.MasterKeyStatus()
	switch {
	case err != nil && count == 0:
		row.OK, row.Detail = false, "vault is unreadable: "+keyErrText(err)
		return []vaultCheck{row}
	case err != nil:
		row.OK, row.Detail = false, fmt.Sprintf("vault holds %d secret(s) but the master key does not open it: %s", count, keyErrText(err))
		return []vaultCheck{row}
	case count == 0:
		return nil
	}
	row.Detail = fmt.Sprintf("%d secret(s); master key from %s", count, source)
	return []vaultCheck{row}
}

// checkRetiredKeys reports whether archive keys remain available in the keyring (advisory).
func checkRetiredKeys() []vaultCheck {
	var id string
	var kids []string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		var err error
		kids, err = vault.Retired(t)
		return err
	}); err != nil {
		return []vaultCheck{{Name: "vault retired keys", Detail: err.Error()}}
	}
	var out []vaultCheck
	for _, kid := range kids {
		row := vaultCheck{Name: "vault retired key " + kid}
		_, found, err := vault.KeyringLookup("vault-key/" + id + "." + kid)
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

// checkKeyNames warns about a stored key that matches no provider's auth.env — likely a typo, but
// not necessarily wrong (a provider not yet in the registry), so this never fails the gate.
func checkKeyNames(reg *providers.Registry) []vaultCheck {
	if reg == nil { // no registry to compare against: every key would read as a typo
		return nil
	}
	known := map[string]bool{providers.OAuthTokenEnv: true}
	for _, n := range tailscale.CredentialNames {
		known[n] = true
	}
	for _, p := range reg.Providers {
		if p.Auth.Env != "" {
			known[p.Auth.Env] = true
		}
	}
	names := vault.Names()
	var typos []string
	for _, k := range names {
		if known[k] {
			continue
		}
		// Only a near miss of a known name is a likely typo; other names (JIRA_TOKEN, …) are the user's own.
		for _, n := range slices.Sorted(maps.Keys(known)) {
			if editDistance(k, n) <= 2 {
				typos = append(typos, k+" (did you mean "+n+"?)")
				break
			}
		}
	}
	if len(typos) == 0 {
		return []vaultCheck{{Name: "vault key names", OK: true, Detail: fmt.Sprintf("%d name(s), no likely typos", len(names))}}
	}
	return []vaultCheck{{Name: "vault key names", Detail: strings.Join(typos, ", ")}}
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
func checkKeyringTool() vaultCheck {
	if vault.KeyringAvailable() {
		return vaultCheck{Name: "keyring tool", OK: true, Detail: vault.KeyringTool() + " on PATH"}
	}
	return vaultCheck{Name: "keyring tool", OK: true, Detail: "none on PATH — set HARNESS_VAULT_KEY or the vault key_command"}
}

// keyCmdStderr matches the key_command stderr tail vault appends to its error; it could echo key material.
var keyCmdStderr = regexp.MustCompile(`(?s)(key_command: [^:]*): .*`)

// keyErrText is err's text with any key_command stderr withheld, safe for doctor text and JSON rows.
func keyErrText(err error) string {
	return keyCmdStderr.ReplaceAllString(err.Error(), "$1 (stderr withheld)")
}
