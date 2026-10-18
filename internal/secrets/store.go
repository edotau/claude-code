package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/keyring"
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
	if err := rewriteFile(func(cur map[string]string) { cur[name] = value }); err != nil {
		return err
	}
	audit.Log(audit.SecretSet, name, "file") // the name only, never the value
	return nil
}

// removeFile drops name from the file plane; a name it does not hold is a no-op.
func removeFile(name string) error {
	if _, ok := ReadAll()[name]; !ok {
		return nil
	}
	return rewriteFile(func(cur map[string]string) { delete(cur, name) })
}

// rewriteFile applies edit to the file plane's pairs and writes them back sorted, under the shared lock.
func rewriteFile(edit func(map[string]string)) error {
	file := EnvFile()
	return paths.WithFileLock(file, func() error {
		cur := providers.ReadEnvFile(file)
		edit(cur)
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

// IndexFile lists, one per line, the NAMES of secrets stored through Store (never values): the keyring can't be
// enumerated cheaply, so a custom secret would otherwise vanish from the dialog once it leaves the file plane.
func IndexFile() string { return filepath.Join(paths.EnvDir(), "secrets.index") }

// Names is every name in the index, sorted; a missing index is empty.
func Names() []string {
	b, err := os.ReadFile(IndexFile())
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Fields(string(b)) {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// editIndex adds (add=true) or drops name in the index, under its own lock.
func editIndex(name string, add bool) error {
	file := IndexFile()
	return paths.WithFileLock(file, func() error {
		names := slices.DeleteFunc(Names(), func(n string) bool { return n == name })
		if add {
			names = append(names, name)
			sort.Strings(names)
		}
		return paths.AtomicWrite(file, []byte(strings.Join(names, "\n")+"\n"), 0o600)
	})
}

// Store writes name to the OS keyring and drops any plaintext copy from the file plane; with no
// usable keyring (or forceFile) it writes the file plane instead. plane names where it landed.
func Store(name, value string, forceFile bool) (plane string, err error) {
	if !forceFile && keyring.Available() {
		if err := keyring.Set(name, value); err != nil {
			return "", err
		}
		plane = "the OS keyring"
		if err := removeFile(name); err != nil {
			return plane, fmt.Errorf("%s stored in the keyring, but %s still holds a plaintext copy: %w", name, EnvFile(), err)
		}
	} else {
		plane = EnvFile()
		if err := SetFile(name, value); err != nil {
			return "", err
		}
	}
	return plane, editIndex(name, true)
}

// Delete removes name from both managed stores; the plaintext line goes first so a refused keyring never keeps it.
func Delete(name string) error {
	if err := removeFile(name); err != nil {
		return err
	}
	if err := keyring.Delete(name); err != nil {
		return err
	}
	return editIndex(name, false)
}

// Redact renders a value for display: a short head and tail plus its length, never the middle.
func Redact(v string) string {
	if len(v) < 32 {
		return fmt.Sprintf("%d chars", len(v))
	}
	return fmt.Sprintf("%s…%s, %d chars", v[:8], v[len(v)-4:], len(v))
}

// Get resolves name through the managed ladder: OS keyring, then the file plane, then the process
// env. source names which plane answered ("keyring"|"file"|"env"); ok is false when none hold it.
func Get(name string) (value, source string, ok bool) {
	if v, found := keyring.Get(name); found {
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
