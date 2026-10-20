package providers

import (
	"sort"

	"github.com/edotau/claude-code/internal/vault"
)

// KeyringRef marks a custom env value held in the OS keyring under the variable's own name.
const KeyringRef = "@keyring"

// CustomEnv is the raw vault env bucket (values unresolved); a read error yields an empty table.
func CustomEnv() map[string]string {
	m := map[string]string{}
	_ = vault.View(func(tx *vault.Tx) error { m = tx.All(vault.Env); return nil })
	return m
}

// ResolveCustomEnv resolves @keyring refs through get; a ref get cannot resolve is listed in missing, not set.
func ResolveCustomEnv(get func(string) (string, bool)) (vals map[string]string, missing []string) {
	vals = map[string]string{}
	for k, v := range CustomEnv() {
		if v != KeyringRef {
			vals[k] = v
			continue
		}
		if s, ok := get(k); ok {
			vals[k] = s
		} else {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return vals, missing
}

// SetCustomEnv writes (raw != "") or deletes (raw == "") name in the vault env bucket, raw and unquoted.
func SetCustomEnv(name, raw string) error {
	return vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Env, name, raw) })
}
