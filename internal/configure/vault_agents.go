// vault_agents.go — the per-agent records ride in the exported doc's "agents" section under "records".
package configure

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/edotau/claude-code/internal/agent"
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
	if len(overlay) != 0 {
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

// splitAgentRecords separates the "records" key from the agents overlay; absent means no records.
func splitAgentRecords(raw json.RawMessage) (json.RawMessage, map[string]vault.AgentRecord, error) {
	raw = presentJSON(raw)
	var top map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &top) != nil {
		return raw, nil, nil // a malformed overlay is reported by validateOverlaySections
	}
	rec, ok := top[recordsKey]
	if !ok {
		return raw, nil, nil
	}
	recs, err := decodeAgentRecords(rec)
	if err != nil {
		return nil, nil, sectionError("agents."+recordsKey, err)
	}
	delete(top, recordsKey)
	if len(top) == 0 {
		return nil, recs, nil
	}
	rest, err := json.Marshal(top)
	return rest, recs, err
}

// decodeAgentRecords strictly decodes raw as agent name -> record; every name must be a roster agent.
func decodeAgentRecords(raw json.RawMessage) (map[string]vault.AgentRecord, error) {
	var recs map[string]vault.AgentRecord
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&recs); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	for name := range recs {
		if _, ok := agent.LookupSchema(name); !ok {
			return nil, fmt.Errorf("unknown agent %q", name)
		}
	}
	return recs, nil
}

// applyAgentRecords stores next when it differs from cur.
func applyAgentRecords(t *vault.Tx, cur, next map[string]vault.AgentRecord) error {
	if len(cur) == 0 && len(next) == 0 || reflect.DeepEqual(cur, next) {
		return nil
	}
	return t.PutAgentRecords(next)
}
