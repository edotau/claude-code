package providers

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// KeyringRef marks a custom env value held in the OS keyring under the variable's own name.
const KeyringRef = "@keyring"

// CustomEnvFile holds user env vars injected into every launch: KEY=value, or KEY=@keyring for a keyring value.
func CustomEnvFile() string { return filepath.Join(paths.EnvDir(), "custom.env") }

// CustomEnv is the raw custom.env table (values unresolved).
func CustomEnv() map[string]string { return ReadEnvFile(CustomEnvFile()) }

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

// SetCustomEnv writes (raw != "") or deletes (raw == "") name in custom.env under its lock.
func SetCustomEnv(name, raw string) error {
	file := CustomEnvFile()
	return paths.WithFileLock(file, func() error {
		cur := ReadEnvFile(file)
		if raw == "" {
			delete(cur, name)
		} else {
			cur[name] = raw
		}
		keys := make([]string, 0, len(cur))
		for k := range cur {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("# Managed by `claude-code configure` → settings → environment; injected into every launch.\n")
		for _, k := range keys {
			b.WriteString(k + "=" + quoteEnv(cur[k]) + "\n")
		}
		return paths.AtomicWrite(file, []byte(b.String()), 0o600)
	})
}

// quoteEnv single-quotes a value with spaces or # so ReadEnvFile reads it back intact.
func quoteEnv(v string) string {
	if strings.ContainsAny(v, " \t#") && !strings.Contains(v, "'") {
		return "'" + v + "'"
	}
	return v
}
