package vault

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// Names returns sorted vault secret names without resolving a key; an unreadable vault yields nil.
func Names() []string {
	names, _ := NamesErr()
	return names
}

// NamesErr is Names with the vault read error surfaced, for callers (doctor --secrets) that must not
// mistake an unreadable vault for an empty one.
func NamesErr() ([]string, error) {
	var names []string
	err := View(func(tx *Tx) error {
		names = tx.Names(Secrets)
		return nil
	})
	return names, err
}

// Store seals a single-line value in the vault, replacing its metadata and clearing any cached probe.
func Store(name, value string) error {
	value = strings.TrimSpace(value) // a paste's leading/trailing spaces and newline are debris, never part of a secret
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s: a secret must be one line", name)
	}
	if err := UpdateSealed(func(tx *Tx, key *Key) error {
		return tx.PutRecord(name, Record{
			Sealed: key.Seal(name, value),
			Kid:    key.Kid,
			Prefix: MetaPrefix(value),
			Len:    len(value),
			SetAt:  time.Now(),
		})
	}); err != nil {
		return err
	}
	audit.Log(audit.SecretSet, name, "vault")
	return nil
}

// Delete removes name's vault record without resolving a key; an absent name is a no-op.
func Delete(name string) error {
	return Update(func(tx *Tx) error {
		return tx.Put(Secrets, name, "")
	})
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
func Preview(v string) string {
	width := previewStars
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

// Lookup reads the vault before the process env; a vault read error (locked, wrong key) falls back to
// env rather than discarding an exported value, and only errors when env is empty too.
func Lookup(name string) (value, source string, err error) {
	v, found, err := ReadSecret(name)
	if err != nil {
		if ev := strings.TrimSpace(os.Getenv(name)); ev != "" {
			return ev, "env", nil
		}
		return "", "", err
	}
	if found {
		return v, "vault", nil
	}
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, "env", nil
	}
	return "", "", nil
}

// Get is Lookup's found-only form; a read error or empty value yields ok=false.
func Get(name string) (value, source string, ok bool) {
	value, source, err := Lookup(name)
	return value, source, err == nil && value != ""
}

// Effective reads env before vault; shadowed reports a vault record hidden by the env value, without a key.
func Effective(name string) (value, source, shadowed string, ok bool) {
	value, source, err := EffectiveSecret(name)
	if err != nil || value == "" {
		return "", "", "", false
	}
	if source == "env" {
		var found bool
		if err := View(func(tx *Tx) error {
			_, found = tx.Get(Secrets, name)
			return nil
		}); err == nil && found {
			shadowed = "vault"
		}
	}
	return value, source, shadowed, true
}

// Locate computes env and vault presence using already-loaded metadata, without resolving a key.
func Locate(name string, meta map[string]SecretMeta) (source, shadowed string) {
	_, found := meta[name]
	if strings.TrimSpace(os.Getenv(name)) != "" {
		if found {
			return "env", "vault"
		}
		return "env", ""
	}
	if found {
		return "vault", ""
	}
	return "", ""
}

// EffectiveSecret reads the process env, then the vault, preserving vault read errors.
func EffectiveSecret(name string) (value, source string, err error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, "env", nil
	}
	v, found, err := ReadSecret(name)
	if err != nil {
		return "", "", err
	}
	if found {
		return v, "vault", nil
	}
	return "", "", nil
}

// HasSecretErr reports env or vault presence without resolving a master key, surfacing the vault read error for
// callers (configure provider --list) that must not mistake an unreadable vault for "no credential".
func HasSecretErr(name string) (source string, err error) {
	if strings.TrimSpace(os.Getenv(name)) != "" {
		return "env", nil
	}
	found := false
	err = View(func(tx *Tx) error {
		_, found = tx.Get(Secrets, name)
		return nil
	})
	if err != nil {
		return "", err
	}
	if found {
		return "vault", nil
	}
	return "", nil
}

// LookupAll resolves each name (vault, then process env); the first unset or unreadable name is the error.
func LookupAll(names []string) ([]string, error) {
	values := make([]string, len(names))
	for i, name := range names {
		value, _, err := Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		if value == "" {
			return nil, fmt.Errorf("%s not set (checked the vault and the process env)", name)
		}
		values[i] = value
	}
	return values, nil
}

// Remove deletes the sealed secret name and audits it; an unknown name is an error.
func Remove(name string) error {
	names, err := NamesErr()
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("%s not set", name)
	}
	if err := Delete(name); err != nil {
		return err
	}
	audit.Log(audit.SecretDelete, name, "")
	return nil
}
