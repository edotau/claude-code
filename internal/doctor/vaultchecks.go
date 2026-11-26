package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/memory/shadow"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	secretredact "github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/sqlite"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
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
	out = append(out, checkMisfiled()...)
	out = append(out, checkLegacy()...)
	return append(out, checkKeyringTool())
}

// checkLegacy names each stored overlay section nothing reads and a stored record key no field takes (advisory).
func checkLegacy() []vaultCheck {
	var docs map[string]string
	if err := vault.View(func(t *vault.Tx) error { docs = t.All(vault.Overlay); return nil }); err != nil {
		return nil // the master key row reports an unreadable vault
	}
	var out []vaultCheck
	for _, k := range vaultdoc.LegacyOverlayKeys {
		doc, ok := docs[k]
		if !ok {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(doc), &m) // not a JSON object: counts 0 keys
		out = append(out, vaultCheck{Name: "vault legacy overlay " + k, Detail: fmt.Sprintf("overlay %s: %d keys "+
			"ignored since 243830d0e; move them with `claude-code configure agents` or remove with "+
			"`claude-code vault rm overlay %s`", k, leafKeys(m), k)})
	}
	if err := vault.CheckRecordKeys([]byte(docs[vault.AgentRecordsKey])); err != nil {
		out = append(out, vaultCheck{Name: "vault legacy record key", Detail: "agent-records: " + err.Error() +
			" is ignored and dropped by the next record write; set its value under a valid key with " +
			"`claude-code configure agents`, or store a cleaned copy with " +
			"`claude-code vault set overlay agent-records --file PATH`"})
	}
	return out
}

// leafKeys counts the non-object values under every key of m, nested objects included.
func leafKeys(m map[string]any) int {
	n := 0
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			n += leafKeys(sub)
		} else {
			n++
		}
	}
	return n
}

// permsGOOS decides whether mode bits mean anything; a seam so tests run the Windows branch on any host.
var permsGOOS = runtime.GOOS

// loosePerm reports a file open beyond its owner; Windows has no such bits (Stat reports 0666), as vault's looseMode.
func loosePerm(fi os.FileInfo) bool { return permsGOOS != "windows" && fi.Mode().Perm()&0o077 != 0 }

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
		row.Detail = fmt.Sprintf("%s is %04o", file, fi.Mode().Perm())
	}
	return []vaultCheck{row}
}

// checkMasterKey fails when the master key cannot open the vault's secrets or the vault is unreadable.
func checkMasterKey() []vaultCheck {
	row := vaultCheck{Name: "vault master key", OK: true, Gate: true}
	count, source, err := vault.MasterKeyStatus()
	switch {
	case err != nil && count == 0:
		row.OK, row.Detail = false, "vault is unreadable: "+err.Error()
		return []vaultCheck{row}
	case err != nil:
		row.OK, row.Detail = false, fmt.Sprintf("vault holds %d secret(s) but the master key does not open it: %s", count, err)
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

// checkKeyNames warns about a stored key matching no provider's auth.env (likely a typo); never fails the gate.
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
	names, err := vault.NamesErr()
	if err != nil {
		return []vaultCheck{{Name: "vault key names", Detail: "vault unreadable: " + err.Error()}}
	}
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

// checkMisfiled names credential records holding a model id; redaction skips them, so they are never masked.
func checkMisfiled() []vaultCheck {
	recs, err := secretredact.VaultRecords()
	if err != nil { // the master key row reports an unreadable vault
		return nil
	}
	var out []vaultCheck
	for _, name := range slices.Sorted(maps.Keys(recs)) {
		if secretredact.IsModelID(recs[name]) {
			out = append(out, vaultCheck{Name: "misfiled: " + name, Detail: name + " holds a model id, not a secret " +
				"(move it: claude-code vault set env " + name + " <value> or configure agents …)"})
		}
	}
	return out
}

// dbProtectRow lists paths.ProtectedDBs, the list paths.Protected checks, so it cannot drift from the rule.
func dbProtectRow() check {
	state := filepath.Join(paths.ConfigDir(), "state")
	var parts []string
	for _, p := range paths.ProtectedDBs() {
		name, _ := filepath.Rel(state, p)
		size := "absent"
		if fi, err := os.Stat(p); err == nil {
			size = textutil.HumanBytes(fi.Size())
		}
		parts = append(parts, filepath.ToSlash(name)+" "+size)
	}
	return check{Name: "state dbs", OK: true, Detail: "never removed by clean, sweep or nuke: " + strings.Join(parts, ", ")}
}

// dbVersionRows reports each existing state/*.db; it opens SQLite read-only and never creates a file.
func dbVersionRows() []check {
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "state", "*.db"))
	rows := make([]check, 0, len(files))
	for _, f := range files {
		rows = append(rows, dbVersionRow(f))
	}
	return rows
}

func dbVersionRow(path string) check {
	name := filepath.Base(path)
	row := check{Name: "db version " + name}
	if name == "vault.db" {
		row.OK, row.Detail = true, "bbolt, no schema version"
		return row
	}
	if !isSQLite(path) {
		row.OK, row.Detail = true, "not SQLite, no schema version"
		return row
	}
	db, err := sqlite.Open(path, sqlite.Options{ReadOnly: true})
	if err != nil {
		row.Detail = "unreadable: " + err.Error()
		return row
	}
	defer db.Close()
	have, err := sqlite.Version(db)
	switch code := shadow.SchemaVersion(); {
	case err != nil:
		row.Detail = "user_version unreadable: " + err.Error()
	case name != "memory.db":
		row.OK, row.Detail = true, fmt.Sprintf("SQLite v%d; this build has no migrations for it", have)
	case have < code:
		row.Detail = fmt.Sprintf("warn v%d, %d behind v%d; claude-code memory shadow --rebuild", have, code-have, code)
	case have > code:
		row.Detail = fmt.Sprintf("v%d newer than code v%d; install a newer claude-code", have, code)
	default:
		row.OK, row.Detail = true, fmt.Sprintf("v%d matches code", have)
	}
	return row
}

// isSQLite is true for an empty file or one with the SQLite header.
func isSQLite(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	return n == 0 || string(head[:n]) == "SQLite format 3\x00"
}
