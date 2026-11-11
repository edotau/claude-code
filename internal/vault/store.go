package vault

import (
	"fmt"
	"os"
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

// Delete removes name's vault record without resolving a key and audits it; an absent name is a silent no-op.
func Delete(name string) error {
	_, err := deleteSecret(name)
	return err
}

// deleteSecret removes name's record in one transaction, audits it when present and reports whether it existed.
func deleteSecret(name string) (bool, error) {
	var existed bool
	if err := Update(func(tx *Tx) error {
		_, existed = tx.Get(Secrets, name)
		return tx.Put(Secrets, name, "")
	}); err != nil {
		return false, err
	}
	if existed {
		audit.Log(audit.SecretDelete, name, "")
	}
	return existed, nil
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
	ev, envOK := envSet(name)
	if err != nil {
		if envOK {
			return ev, "env", nil
		}
		return "", "", err
	}
	if found {
		return v, "vault", nil
	}
	if envOK {
		return ev, "env", nil
	}
	return "", "", nil
}

// envSet returns the trimmed process env value of name and whether it is non-empty.
func envSet(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

// vaultHas reports whether the vault holds a sealed record for name, without resolving a master key.
func vaultHas(name string) (found bool, err error) {
	err = View(func(tx *Tx) error {
		_, found = tx.Get(Secrets, name)
		return nil
	})
	return found, err
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
		if found, err := vaultHas(name); err == nil && found {
			shadowed = "vault"
		}
	}
	return value, source, shadowed, true
}

// Locate computes env and vault presence using already-loaded metadata, without resolving a key.
func Locate(name string, meta map[string]SecretMeta) (source, shadowed string) {
	_, found := meta[name]
	if _, ok := envSet(name); ok {
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
	if v, ok := envSet(name); ok {
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
	if _, ok := envSet(name); ok {
		return "env", nil
	}
	found, err := vaultHas(name)
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
	batch, batchErr := ReadSecrets(names) // one vault open; on error each name falls back to Lookup
	values := make([]string, len(names))
	for i, name := range names {
		var value string
		var err error
		if v, found := batch[name]; batchErr == nil && found {
			value = v
		} else if batchErr == nil {
			value = strings.TrimSpace(os.Getenv(name))
		} else {
			value, _, err = Lookup(name)
		}
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
	existed, err := deleteSecret(name)
	if err != nil {
		return err
	}
	if !existed {
		return fmt.Errorf("%s not set", name)
	}
	return nil
}

// SecretMeta is a vault record's display and probe metadata, without ciphertext or key information.
type SecretMeta struct {
	Prefix       string    `json:"prefix,omitempty"`
	Len          int       `json:"len,omitempty"`
	SetAt        time.Time `json:"set_at,omitzero"`
	VerifiedAt   time.Time `json:"verified_at,omitzero"`
	VerifyStatus string    `json:"verify_status,omitempty"`
}

// LoadMeta reads every record's metadata without resolving a master key.
func LoadMeta() (map[string]SecretMeta, error) {
	out := map[string]SecretMeta{}
	err := View(func(tx *Tx) error {
		records, err := tx.Records()
		if err != nil {
			return err
		}
		for name, r := range records {
			out[name] = SecretMeta{
				Prefix:       r.Prefix,
				Len:          r.Len,
				SetAt:        r.SetAt,
				VerifiedAt:   r.VerifiedAt,
				VerifyStatus: r.VerifyStatus,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordProbe updates probe metadata in one transaction, preserving the sealed value without needing its key.
// sinceSet is the record's SetAt captured by the caller before the (slow) probe ran; if the record's SetAt
// has since moved — a concurrent `vault set` replaced the value mid-probe — the stale result is dropped.
func RecordProbe(name, status string, at, sinceSet time.Time) error {
	return Update(func(tx *Tx) error {
		r, found, err := tx.Record(name)
		if err != nil || !found {
			return err
		}
		if !r.SetAt.Equal(sinceSet) {
			return nil // the value changed under the probe; its stale verdict must not stamp the new one
		}
		r.VerifyStatus, r.VerifiedAt = status, at
		return tx.PutRecord(name, r)
	})
}

// CachedProbe reports m's cached verify result if it is still within ttl: ok is true only for
// verified/rejected; age is always now−VerifiedAt whenever VerifiedAt is set, cached or not.
func CachedProbe(m SecretMeta, ttl time.Duration, now time.Time) (status string, age time.Duration, ok bool) {
	if m.VerifiedAt.IsZero() {
		return "", 0, false
	}
	age = now.Sub(m.VerifiedAt)
	if age >= 0 && (m.VerifyStatus == "verified" || m.VerifyStatus == "rejected") && age < ttl {
		return m.VerifyStatus, age, true
	}
	return "", age, false
}

// MetaPrefix retains a vendor prefix only when it covers at most a third of the secret.
func MetaPrefix(v string) string {
	if p := keyPrefix(v); p != "" && len(p) <= len(v)/3 {
		return p
	}
	if len(v) >= redactMin { // no vendor scheme: the same first chars Redact shows, never over a third
		return v[:min(redactShow, len(v)/3)]
	}
	return ""
}
