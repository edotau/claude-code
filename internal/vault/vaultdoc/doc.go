package vaultdoc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/vault"
)

// Doc is the non-secret vault view (no secrets, no providers.prev); Agents carries "records" (D-a).
type Doc struct {
	Pins      map[string]string `json:"pins"`
	Env       map[string]string `json:"env"`
	Providers json.RawMessage   `json:"providers"`
	Agents    json.RawMessage   `json:"agents"`
	Settings  json.RawMessage   `json:"settings"`
	Vault     struct {
		KeyCommand string `json:"key_command"`
	} `json:"vault"`
}

// docFrom builds the doc inside an open transaction; it never opens the vault itself.
func docFrom(t *vault.Tx) (Doc, error) {
	d := Doc{Pins: t.All(vault.Pins), Env: t.All(vault.Env)}
	for name, dst := range map[string]*json.RawMessage{"providers": &d.Providers, "agents": &d.Agents, "settings": &d.Settings} {
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

// render is the canonical indented doc bytes.
func render(d Doc) ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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
	return render(d)
}

// presentJSON maps a JSON null section to absent.
func presentJSON(raw json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
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
