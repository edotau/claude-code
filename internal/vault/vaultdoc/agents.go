// The per-agent records ride in the exported doc's "agents" section under "records".

package vaultdoc

import (
	"encoding/json"
	"fmt"

	"github.com/edotau/claude-code/internal/vault"
)

// recordsKey is the key inside the "agents" section that holds the agent records.
const recordsKey = "records"

// mergeAgentRecords returns the agents overlay with recs under "records"; the overlay as is when recs is empty.
func mergeAgentRecords(overlay json.RawMessage, recs map[string]vault.AgentRecord) (json.RawMessage, error) {
	if len(recs) == 0 {
		return overlay, nil
	}
	top := map[string]json.RawMessage{}
	if overlay = presentJSON(overlay); len(overlay) != 0 { // a JSON null would decode top to a nil map
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
