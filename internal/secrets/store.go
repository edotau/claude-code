package secrets

import (
	"os"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// EnvFile is the managed KEY=value store (env.d/secrets.env): 0600, gitignored.
func EnvFile() string { return providers.SecretsFile() }

// ReadAll returns every KEY=value pair currently in the file plane; a missing file is empty.
func ReadAll() map[string]string { return providers.ReadEnvFile(EnvFile()) }

// SetFile writes (or replaces) name=value in the file plane, chmod 600, atomic, under a file lock
// shared with any concurrent `secrets set`.
func SetFile(name, value string) error {
	file := EnvFile()
	return paths.WithFileLock(file, func() error {
		cur := providers.ReadEnvFile(file)
		cur[name] = value
		keys := make([]string, 0, len(cur))
		for k := range cur {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("# Managed by `claude-code secrets set`. KEY=value, one per line, 0600.\n")
		for _, k := range keys {
			b.WriteString(k + "=" + cur[k] + "\n")
		}
		return paths.AtomicWrite(file, []byte(b.String()), 0o600)
	})
}

// Get resolves name through the managed ladder: OS keyring, then the file plane, then the process
// env. source names which plane answered ("keyring"|"file"|"env"); ok is false when none hold it.
func Get(name string) (value, source string, ok bool) {
	if v, found := KeyringGet(name); found {
		return v, "keyring", true
	}
	if v, found := ReadAll()[name]; found && v != "" {
		return v, "file", true
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, "env", true
	}
	return "", "", false
}
