package settings

import (
	"encoding/json"
	"fmt"

	"github.com/edotau/claude-code/internal/vault"
)

// hooksBucketKey is the vault Overlay entry holding the harness hook-group switches: group name -> enabled.
const hooksBucketKey = "harnessHooks"

// LoadHarnessHooks reads the group switches; an absent entry is an empty map (every group on).
func LoadHarnessHooks() (map[string]bool, error) {
	m := map[string]bool{}
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, hooksBucketKey)
		if !ok || raw == "" {
			return nil
		}
		return json.Unmarshal([]byte(raw), &m)
	})
	if err != nil {
		return nil, fmt.Errorf("vault overlay/%s: %w", hooksBucketKey, err)
	}
	// The docs group was named plans; a stored plans switch is vault state, so it is read as docs.
	if on, ok := m["plans"]; ok {
		if _, set := m["docs"]; !set {
			m["docs"] = on
		}
		delete(m, "plans")
	}
	return m, nil
}

// SaveHarnessHooks validates group names (a gate group cannot be switched off) and stores m; empty deletes the entry.
func SaveHarnessHooks(m map[string]bool) error {
	for name, on := range m {
		var found, gate bool
		for _, g := range Registry {
			if g.Name == name {
				found, gate = true, g.Gate
			}
		}
		if !found {
			return fmt.Errorf("harnessHooks: unknown group %q", name)
		}
		if gate && !on {
			return fmt.Errorf("harnessHooks: %s is a gate group and stays on", name)
		}
	}
	var raw string
	if len(m) > 0 {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	return vault.Update(func(t *vault.Tx) error { return t.Put(vault.Overlay, hooksBucketKey, raw) })
}
