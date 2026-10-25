// overlaymigrate.go — drops the dead opencode mcp/permission keys from the vault's vendors overlay.
package configure

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/vault"
)

// deadOpencodeKeys are vendors.opencode fields no renderer reads any more; codex/copilot/gemini keep theirs.
var deadOpencodeKeys = []string{"mcp", "permission"}

// MigrateVendorOverlay deletes vendors.opencode.{mcp,permission} from vault overlay/vendors in one transaction.
// It prints one line when something was (or, with dryRun, would be) dropped; a clean or absent overlay is a no-op.
func MigrateVendorOverlay(dryRun bool, out io.Writer) error {
	var dropped bool
	run := vault.Update
	if dryRun {
		run = vault.View
	}
	err := run(func(tx *vault.Tx) error {
		old, ok := tx.Get(vault.Overlay, "vendors")
		if !ok || old == "" {
			return nil
		}
		next, changed, err := dropDeadOpencodeKeys([]byte(old))
		if err != nil || !changed {
			return err
		}
		dropped = true
		if dryRun {
			return nil
		}
		return tx.Put(vault.Overlay, "vendors", string(next))
	})
	if err != nil {
		return fmt.Errorf("vault overlay/vendors: %w", err)
	}
	if dropped {
		fmt.Fprintln(out, "vault: dropped dead opencode mcp/permission overlay keys")
	}
	return nil
}

// dropDeadOpencodeKeys returns doc without vendors.opencode's dead keys, and whether any was present.
func dropDeadOpencodeKeys(doc []byte) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return nil, false, err
	}
	var vendors map[string]json.RawMessage
	if raw, ok := top["vendors"]; !ok || json.Unmarshal(raw, &vendors) != nil {
		return doc, false, nil
	}
	var oc map[string]json.RawMessage
	if raw, ok := vendors["opencode"]; !ok || json.Unmarshal(raw, &oc) != nil || oc == nil {
		return doc, false, nil
	}
	changed := false
	for _, k := range deadOpencodeKeys {
		if _, ok := oc[k]; ok {
			delete(oc, k)
			changed = true
		}
	}
	if !changed {
		return doc, false, nil
	}
	var err error
	if vendors["opencode"], err = json.Marshal(oc); err != nil {
		return nil, false, err
	}
	if top["vendors"], err = json.Marshal(vendors); err != nil {
		return nil, false, err
	}
	next, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(next, '\n'), true, nil
}
