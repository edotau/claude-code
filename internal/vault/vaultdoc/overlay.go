package vaultdoc

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// LegacyOverlayKeys are sections nothing reads since 243830d0e; doctor names any still stored.
var LegacyOverlayKeys = []string{"agents", "codex-settings", "settings"}

// OverlayKeys are removable overlays: documents that only override a default, and the legacy ones.
var OverlayKeys = append([]string{"proxy-teams"}, LegacyOverlayKeys...)

// SettableOverlayKeys excludes backups such as providers.prev.
var SettableOverlayKeys = []string{"proxy-teams", "providers", "agent-records", "opencode-models"}

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
	if !slices.Contains(SettableOverlayKeys, key) {
		return 0, errors.New("not a settable overlay key")
	}
	var old int
	err := vault.Update(func(t *vault.Tx) error {
		cur, _ := t.Get(vault.Overlay, key)
		old = len(cur)
		if key == "providers" {
			if err := providers.ValidateOverlayChange([]byte(cur), []byte(doc)); err != nil {
				return errors.New("provider overlay change refused; nothing written")
			}
		}
		if key == "providers" && cur != "" {
			if err := t.Put(vault.Overlay, "providers.prev", cur); err != nil {
				return err
			}
		}
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
