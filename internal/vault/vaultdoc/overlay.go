package vaultdoc

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// OverlayKeys are the overlay entries `vault overlay set|unset` accept: documents that only override a default.
var OverlayKeys = []string{"proxy-teams"}

func checkOverlayKey(key string) error {
	if !slices.Contains(OverlayKeys, key) {
		return fmt.Errorf("%q is not a removable overlay key (one of %s)", key, strings.Join(OverlayKeys, ", "))
	}
	return nil
}

// PeekOverlay is the stored document of a known overlay key, "" when absent; it writes nothing.
func PeekOverlay(key string) (string, error) {
	if err := checkOverlayKey(key); err != nil {
		return "", err
	}
	var doc string
	err := vault.View(func(t *vault.Tx) error { doc, _ = t.Get(vault.Overlay, key); return nil })
	return doc, err
}

// UnsetOverlay removes key only while it still holds the document the caller saw, so a concurrent edit is never lost.
func UnsetOverlay(key, seen string) error {
	if err := checkOverlayKey(key); err != nil {
		return err
	}
	return vault.Update(func(t *vault.Tx) error {
		if cur, _ := t.Get(vault.Overlay, key); cur != seen {
			return fmt.Errorf("overlay %s changed since it was read; nothing removed, run again", key)
		}
		return t.Put(vault.Overlay, key, "")
	})
}

// SetOverlay stores doc under a known overlay key and returns the previous size in bytes.
func SetOverlay(key, doc string) (int, error) {
	if err := checkOverlayKey(key); err != nil {
		return 0, err
	}
	var old int
	err := vault.Update(func(t *vault.Tx) error {
		cur, _ := t.Get(vault.Overlay, key)
		old = len(cur)
		return t.Put(vault.Overlay, key, doc)
	})
	return old, err
}

// OverlayStored lists each overlay key with its stored size in bytes, sorted by key.
func OverlayStored() (map[string]int, error) {
	out := map[string]int{}
	err := vault.View(func(t *vault.Tx) error {
		for k, v := range t.All(vault.Overlay) {
			out[k] = len(v)
		}
		return nil
	})
	return out, err
}
