package secrets

import (
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

// Meta is a vault record's display and probe metadata, without ciphertext or key information.
type Meta struct {
	Prefix       string    `json:"prefix,omitempty"`
	Len          int       `json:"len,omitempty"`
	SetAt        time.Time `json:"set_at,omitzero"`
	VerifiedAt   time.Time `json:"verified_at,omitzero"`
	VerifyStatus string    `json:"verify_status,omitempty"`
}

// LoadMeta reads every record's metadata without resolving a master key.
func LoadMeta() (map[string]Meta, error) {
	out := map[string]Meta{}
	err := vault.View(func(tx *vault.Tx) error {
		records, err := tx.Records()
		if err != nil {
			return err
		}
		for name, r := range records {
			out[name] = Meta{
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
	return vault.Update(func(tx *vault.Tx) error {
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
func CachedProbe(m Meta, ttl time.Duration, now time.Time) (status string, age time.Duration, ok bool) {
	if m.VerifiedAt.IsZero() {
		return "", 0, false
	}
	age = now.Sub(m.VerifiedAt)
	if (m.VerifyStatus == "verified" || m.VerifyStatus == "rejected") && age < ttl {
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
