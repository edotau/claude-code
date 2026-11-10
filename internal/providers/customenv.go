package providers

import (
	"fmt"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// SecretRef marks a custom env value held in the vault under the variable's own name.
const SecretRef = "@secret"

// SecretRefName is the vault name a value references: "@secret" means key itself, "@secret:<NAME>" names it.
func SecretRefName(key, v string) (string, bool) {
	if v == SecretRef {
		return key, true
	}
	name, ok := strings.CutPrefix(v, SecretRef+":")
	return name, ok && name != ""
}

// CustomEnv is the raw vault env bucket (values unresolved); a read error yields an empty table.
func CustomEnv() map[string]string {
	m := map[string]string{}
	_ = vault.View(func(tx *vault.Tx) error { m = tx.All(vault.Env); return nil })
	return m
}

// ResolveCustomEnv resolves secret refs through get and returns env-bucket read errors.
func ResolveCustomEnv(get func(string) (string, bool)) (vals map[string]string, missing []string, err error) {
	vals = map[string]string{}
	var raw map[string]string
	err = vault.View(func(tx *vault.Tx) error { raw = tx.All(vault.Env); return nil })
	if err != nil {
		return vals, nil, err
	}
	route := RouteSecretGuard()
	for k, v := range raw {
		name, ok := SecretRefName(k, v)
		if !ok {
			vals[k] = v
			continue
		}
		if name != k && (InternalSecretName(name) || route(name)) { // a named ref must not alias past the scrub
			missing = append(missing, k)
			continue
		}
		if s, ok := get(name); ok {
			vals[k] = s
		} else {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return vals, missing, nil
}

// SetCustomEnv writes (raw != "") or deletes (raw == "") name in the vault env bucket, raw and unquoted.
func SetCustomEnv(name, raw string) error {
	if name == "HARNESS_ROUTE" && strings.HasPrefix(raw, "databricks") {
		return fmt.Errorf("HARNESS_ROUTE=%s cannot be stored: the databricks route is a launch flag (--route databricks:<profile>)", raw)
	}
	return vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Env, name, raw) })
}
