package providers

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// RouterSecretName is the vault record of the loopback router's client secret.
const RouterSecretName = "CLAUDE_CODE_ROUTER_SECRET"

// InternalSecretName reports whether a vault name belongs to the harness itself and may never be handed to a child.
func InternalSecretName(name string) bool {
	return name == RouterSecretName || strings.HasPrefix(strings.ToUpper(name), "HARNESS_")
}

// loginSecrets are the agent logins a launch hands only to its own agent (OPENCODE_API_KEY included).
var loginSecrets = []string{OAuthTokenEnv, CopilotTokenEnv, "OPENCODE_API_KEY"}

// routeSecret reports a route credential: an agent login, a profile's auth env or any-method secret, any *_OAUTH/*_M2M.
func (r *Registry) routeSecret(name string) bool {
	if slices.Contains(loginSecrets, name) ||
		strings.HasSuffix(name, "_"+strings.ToUpper(AuthOAuth)) || strings.HasSuffix(name, "_"+strings.ToUpper(AuthM2M)) ||
		strings.HasSuffix(name, "_"+strings.ToUpper(AuthM2M)+"_ACCESS") {
		return true
	}
	for _, p := range r.Providers {
		if p == nil {
			continue
		}
		if p.Auth.Env == name {
			return true
		}
		for _, m := range []string{AuthOAuth, AuthM2M, AuthPAT, AuthKey} {
			if SecretName(p.Provider, p.Name, m) == name {
				return true
			}
		}
	}
	return false
}

// RouteSecretGuard is Registry.routeSecret over one lazy Load; an unreadable registry refuses every name (fail closed).
func RouteSecretGuard() func(string) bool {
	var reg *Registry
	var err error
	return func(name string) bool {
		if reg == nil && err == nil {
			reg, err = Load()
		}
		return err != nil || reg.routeSecret(name)
	}
}

// SecretNames unions every env var the harness itself reads (each enabled provider's auth.env, the
// setup-token) with the names index `vault set` has already written — never a hand copy of either.
func SecretNames() []string {
	var out []string
	if reg, err := Load(); err == nil {
		for _, n := range reg.Names() {
			if env := reg.Providers[n].Auth.Env; env != "" {
				out = append(out, env)
			}
		}
	}
	out = append(out, OAuthTokenEnv)
	out = append(out, vault.Names()...)
	slices.Sort(out)
	return slices.Compact(out)
}

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
	slices.Sort(missing)
	return vals, missing, nil
}

// SetCustomEnv writes (raw != "") or deletes (raw == "") name in the vault env bucket, raw and unquoted.
func SetCustomEnv(name, raw string) error {
	if name == "HARNESS_COWORKER" {
		if err := checkCoworkerEnv(raw); err != nil {
			return err
		}
	}
	return vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Env, name, raw) })
}

// checkCoworkerEnv admits a stored HARNESS_COWORKER of <provider>[:<profile>] for any provider but databricks.
func checkCoworkerEnv(raw string) error {
	provider, profile, named := strings.Cut(raw, ":")
	switch {
	case raw == "":
		return nil
	case strings.HasPrefix(raw, "databricks"):
		return fmt.Errorf("HARNESS_COWORKER=%s cannot be stored: the databricks route is per dispatch (an overlay's provider/profile)", raw)
	case !slices.Contains(providerKeys, provider) || (named && (profile == "" || strings.Contains(profile, ":"))):
		keys := slices.DeleteFunc(ProviderKeys(), func(k string) bool { return k == "databricks" })
		return fmt.Errorf("HARNESS_COWORKER=%s cannot be stored: want <provider>[:<profile>], provider one of %s", raw, strings.Join(keys, "|"))
	}
	return nil
}
