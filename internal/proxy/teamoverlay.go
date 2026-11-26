package proxy

import "github.com/edotau/claude-code/internal/platform/vault"

// TeamOverlay is the vault's proxy-teams overlay document, nil when none is stored or the vault is unreadable.
func TeamOverlay() []byte {
	var overlay []byte
	err := vault.View(func(t *vault.Tx) error {
		if s, ok := t.Get(vault.Overlay, "proxy-teams"); ok {
			overlay = []byte(s)
		}
		return nil
	})
	if err != nil {
		return nil
	}
	return overlay
}
