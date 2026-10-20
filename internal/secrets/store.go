package secrets

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// EnvFile is the managed KEY=value store (env.d/secrets.env): 0600, gitignored.
func EnvFile() string { return paths.SecretsFile() }

// ReadAll returns every KEY=value pair currently in the file plane; a missing file is empty.
func ReadAll() map[string]string { return providers.ReadEnvFile(EnvFile()) }

// setFile writes (or replaces) name=value in the file plane only; the caller records metadata.
func setFile(name, value string) error {
	if strings.ContainsAny(value, "\r\n") { // a newline would split into extra NAME=value lines on read
		return fmt.Errorf("%s: a value stored in %s must be one line", name, EnvFile())
	}
	if err := editStore(func(s *state) { s.file[name] = value }); err != nil {
		return err
	}
	audit.Log(audit.SecretSet, name, "file") // the name only, never the value
	return nil
}

// removeFile drops name from the file plane; a name it does not hold is a no-op.
func removeFile(name string) error {
	return editStore(func(s *state) { delete(s.file, name) })
}

// state is both managed files as read under the store lock: the file plane's pairs and the meta rows.
type state struct {
	file map[string]string
	meta map[string]Meta
}

// editStore is the one write path: load under the store lock, edit, write back (atomic, 0600) only what changed.
func editStore(edit func(*state)) error {
	return withStoreLock(func() error {
		meta, _, metaErr := readMetaLocked()
		if meta == nil {
			meta = map[string]Meta{}
		}
		s := &state{file: ReadAll(), meta: meta}
		file0, meta0 := maps.Clone(s.file), maps.Clone(s.meta)
		edit(s)
		metaChanged := !maps.Equal(meta0, s.meta)
		if metaChanged && metaErr != nil { // a bad meta file fails only meta edits; file-plane writes still land
			return metaErr
		}
		if !maps.Equal(file0, s.file) {
			if err := writeFile(s.file); err != nil {
				return err
			}
		}
		if metaChanged {
			return writeMeta(s.meta)
		}
		return nil
	})
}

// withStoreLock takes both legacy locks in one fixed order, so it serializes with an older binary's per-file locks.
func withStoreLock(fn func() error) error {
	return paths.WithFileLock(EnvFile(), func() error { return paths.WithFileLock(MetaFile(), fn) })
}

// writeFile writes the file plane's pairs back sorted (the caller holds the store lock).
func writeFile(pairs map[string]string) error {
	var b strings.Builder
	b.WriteString("# Managed by `claude-code secrets set`. KEY=value, one per line, 0600.\n")
	for _, k := range slices.Sorted(maps.Keys(pairs)) {
		b.WriteString(k + "=" + pairs[k] + "\n")
	}
	return paths.AtomicWrite(EnvFile(), []byte(b.String()), 0o600)
}

// Names is every known credential name, sorted; an unreadable meta file falls back to the file plane's names.
func Names() []string {
	m, err := LoadMeta()
	if err != nil {
		return slices.Sorted(maps.Keys(ReadAll()))
	}
	return slices.Sorted(maps.Keys(m))
}

// Store writes name to the OS keyring and drops any plaintext copy from the file plane; with no
// usable keyring (or forceFile) it writes the file plane instead. plane names where it landed.
// Metadata is recorded once, after whichever branch lands, never by the file/keyring write itself.
func Store(name, value string, forceFile bool) (plane string, err error) {
	if !forceFile && keyring.Available() {
		if err := keyring.Set(name, value); err != nil {
			return "", err
		}
		plane = "the OS keyring"
		if err := upsertMeta(name, "keyring", value); err != nil { // recorded before cleanup, so a stale probe never survives
			return plane, err
		}
		if err := removeFile(name); err != nil {
			return plane, fmt.Errorf("%s stored in the keyring, but %s still holds a plaintext copy: %w", name, EnvFile(), err)
		}
		return plane, nil
	}
	plane = EnvFile()
	if err := setFile(name, value); err != nil {
		return "", err
	}
	if err := upsertMeta(name, "file", value); err != nil {
		return plane, err
	}
	if forceFile && keyring.Available() { // Get prefers the keyring, so a stale copy there would win
		if err := keyring.Delete(name); err != nil {
			return plane, fmt.Errorf("%s written to %s, but the keyring still holds the old value: %w", name, plane, err)
		}
	}
	return plane, nil
}

// Delete removes name from both managed stores; the plaintext line goes first so a refused keyring never keeps it.
func Delete(name string) error {
	if err := removeFile(name); err != nil {
		return err
	}
	if err := keyring.Delete(name); err != nil {
		return err
	}
	return removeMeta(name)
}

// Redact renders a stored value for display: a 5-char prefix (enough to recognise a paste) plus its length.
func Redact(v string) string {
	if len(v) < redactMin {
		return fmt.Sprintf("%d chars", len(v))
	}
	return fmt.Sprintf("%s*** (%d chars)", v[:redactShow], len(v))
}

// Preview shows a just-pasted value so a wrong or doubled paste is visible: the key's own prefix (up to its last
// -/_ in the first 16 chars, never over a third of it), one * per hidden char up to a cap, and the length.
// A value that is two identical halves is flagged. Fits an 80-column line after a 10-column label.
func Preview(v string) string { return PreviewWithin(v, previewStars) }

// PreviewWithin is Preview with prefix+stars capped at width, for tables that need a narrower column.
func PreviewWithin(v string, width int) string {
	shown := min(4, len(v)/3)
	if p := keyPrefix(v); len(p) > shown && len(p) <= len(v)/3 {
		shown = len(p)
	}
	hidden := len(v) - shown
	stars := strings.Repeat("*", max(0, min(hidden, width-shown)))
	if hidden > width-shown {
		stars += "…"
	}
	out := fmt.Sprintf("%s%s (%d chars)", v[:shown], stars, len(v))
	if n := len(v); n >= 16 && n%2 == 0 && v[:n/2] == v[n/2:] {
		out += " · pasted twice?"
	}
	return out
}

// keyPrefix is a key's vendor prefix: leading short lowercase-alnum segments, each ending in - or _ ("sk-ant-oat01-",
// "tvly-dev-"); it stops at the first segment that looks random (any uppercase, or longer than 6), never exposing it.
func keyPrefix(v string) string {
	end := 0
	for end < len(v) && end < 16 {
		i := strings.IndexAny(v[end:], "-_")
		if i <= 0 || i > 6 || strings.ToLower(v[end:end+i]) != v[end:end+i] {
			break
		}
		end += i + 1
	}
	return v[:end]
}

// previewStars caps prefix+stars so "label  " + preview + length + the doubled-paste flag stay within 80 columns.
const previewStars = 40

// redactMin is the shortest value that shows a prefix; redactShow is how much of it (the Tailscale console shows 5).
const (
	redactMin  = 12
	redactShow = 5
)

// keyringLookup is keyring.Lookup as a package seam; tests never touch the real keychain.
var keyringLookup = keyring.Lookup

// keyringGet is the found?-only shape most callers want, derived from the keyringLookup seam.
func keyringGet(name string) (string, bool) {
	v, found, _ := keyringLookup(name)
	return v, found
}

// Get resolves name through the managed ladder: OS keyring, then the file plane, then the process
// env. source names which plane answered ("keyring"|"file"|"env"); ok is false when none hold it.
// `secrets get`, custom.env's @keyring refs and rotate.planeOf want this order — leave it alone.
func Get(name string) (value, source string, ok bool) {
	if v, found := keyringGet(name); found {
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

// ladder is the one source/shadowed ruling, env then keyring then file, shared by Effective (live
// presence, keyring.Get on the critical path) and Locate (already-loaded meta/file maps, no lookup).
func ladder(envSet, inKeyring, inFile bool) (source, shadowed string) {
	if envSet {
		switch {
		case inKeyring:
			return "env", "keyring"
		case inFile:
			return "env", "file"
		}
		return "env", ""
	}
	if inKeyring {
		return "keyring", ""
	}
	if inFile {
		return "file", ""
	}
	return "", ""
}

// Effective is the order a launch (providers.Credential) actually uses: process env, then keyring,
// then the file plane. When an env var wins, shadowed names a managed plane that ALSO holds a
// (possibly stale) value for the same name, so the caller can flag "env over keyring/file".
func Effective(name string) (value, source, shadowed string, ok bool) {
	value, source = providers.EffectiveSecret(name)
	if value == "" {
		return "", "", "", false
	}
	if source == "env" {
		_, inKeyring := keyringGet(name)
		v, inFile := ReadAll()[name]
		_, shadowed = ladder(true, inKeyring, inFile && v != "")
	}
	return value, source, shadowed, true
}

// Locate is Effective's source/shadowed computation against already-loaded metadata and file-plane
// maps instead of spawning keyring.Get: a keyring row is taken on faith from meta, no live lookup.
func Locate(name string, meta map[string]Meta, file map[string]string) (source, shadowed string) {
	envSet := strings.TrimSpace(os.Getenv(name)) != ""
	return ladder(envSet, meta[name].Plane == "keyring", file[name] != "")
}
