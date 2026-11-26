package vaultdoc

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/vault"
)

// Doc is the non-secret vault view (no secrets, no providers.prev); Agents carries "records" (D-a).
type Doc struct {
	Env       map[string]string `json:"env"`
	Providers json.RawMessage   `json:"providers"`
	Agents    json.RawMessage   `json:"agents"`
	Vault     struct {
		KeyCommand string `json:"key_command"`
	} `json:"vault"`
}

// recordsKey is the key inside the "agents" section that holds the agent records.
const recordsKey = "records"

// mergeAgentRecords returns the agents overlay with recs under "records"; the overlay as is when recs is empty.
func mergeAgentRecords(overlay json.RawMessage, recs map[string]vault.AgentRecord) (json.RawMessage, error) {
	if len(recs) == 0 {
		return overlay, nil
	}
	top := map[string]json.RawMessage{}
	if !isBlankJSON(overlay) { // a JSON null would decode top to a nil map
		if err := json.Unmarshal(overlay, &top); err != nil {
			return nil, fmt.Errorf("vault overlay/agents: %w", err)
		}
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		return nil, err
	}
	top[recordsKey] = raw
	return json.Marshal(top)
}

// docFrom builds the doc inside an open transaction; it never opens the vault itself.
func docFrom(t *vault.Tx) (Doc, error) {
	d := Doc{Env: t.All(vault.Env)}
	for name, dst := range map[string]*json.RawMessage{"providers": &d.Providers, "agents": &d.Agents} {
		if raw, ok := t.Get(vault.Overlay, name); ok {
			if !json.Valid([]byte(raw)) {
				return Doc{}, fmt.Errorf("vault overlay/%s: invalid JSON", name)
			}
			*dst = json.RawMessage(raw)
		}
	}
	recs, err := t.AgentRecords()
	if err != nil {
		return Doc{}, err
	}
	if d.Agents, err = mergeAgentRecords(d.Agents, recs); err != nil {
		return Doc{}, err
	}
	d.Vault.KeyCommand, _ = t.Get(vault.Meta, "key_command")
	return d, nil
}

// Export renders the vault's non-secret config as indented JSON.
func Export() ([]byte, error) {
	var d Doc
	err := vault.View(func(t *vault.Tx) (err error) {
		d, err = docFrom(t)
		return err
	})
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// RotateKey retains the old key so archived copies of this vault remain readable.
func RotateKey(w io.Writer, force bool) error {
	old, next, err := vault.RotateKey(force)
	if err != nil {
		return err
	}
	var id string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w,
		"rotated vault key %s -> %s; old key retained as vault-key/%s.%s\n",
		old, next, id, old)
	return err
}
