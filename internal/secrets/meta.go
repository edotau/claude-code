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
func RecordProbe(name, status string, at time.Time) error {
	return vault.Update(func(tx *vault.Tx) error {
		r, found, err := tx.Record(name)
		if err != nil || !found {
			return err
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

// metaPrefix is the vendor prefix safe to persist: dropped when it covers over a third of v, so a value made only
// of short dash-separated segments never lands in the meta file whole.
func metaPrefix(v string) string {
	if p := keyPrefix(v); len(p) <= len(v)/3 {
		return p
	}
	return ""
}
