// The per-agent records ride in the exported doc's "agents" section under "records".

package vaultdoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"

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

// cutRecords separates the raw "records" value from the rest of the agents overlay; absent means nil.
func cutRecords(raw json.RawMessage) (rest, recs json.RawMessage, err error) {
	raw = presentJSON(raw)
	var top map[string]json.RawMessage
	if len(raw) == 0 {
		return nil, nil, nil
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, err
	}
	recs, ok := top[recordsKey]
	if !ok {
		return raw, nil, nil
	}
	delete(top, recordsKey)
	if len(top) == 0 {
		return nil, recs, nil
	}
	rest, err = json.Marshal(top)
	return rest, recs, err
}

// splitAgentRecords separates the "records" key from the agents overlay; absent means no records.
func splitAgentRecords(raw json.RawMessage) (json.RawMessage, map[string]vault.AgentRecord, error) {
	rest, rec, err := cutRecords(raw)
	if err != nil || rec == nil {
		return presentJSON(raw), nil, nil // a malformed overlay is reported by the edited doc's validation
	}
	recs, err := decodeAgentRecords(rec)
	if err != nil {
		return nil, nil, sectionError("agents."+recordsKey, err)
	}
	return rest, recs, nil
}

// decodeAgentRecords strictly decodes raw as agent name -> record; every name must be a roster agent.
func decodeAgentRecords(raw json.RawMessage) (map[string]vault.AgentRecord, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	var recs map[string]vault.AgentRecord
	if entries != nil {
		recs = make(map[string]vault.AgentRecord, len(entries))
	}
	for name, r := range entries {
		rec, err := decodeAgentRecord(name, r)
		if err != nil {
			return nil, err
		}
		recs[name] = rec
	}
	return recs, nil
}

// decodeAgentRecord strictly decodes one roster agent's record: unknown fields and trailing data are errors.
func decodeAgentRecord(name string, raw json.RawMessage) (vault.AgentRecord, error) {
	var rec vault.AgentRecord
	if _, ok := agent.LookupSchema(name); !ok {
		return rec, fmt.Errorf("unknown agent %q", name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return rec, errors.New("trailing data")
	}
	return rec, nil
}

// agentRecords decodes the edited records one agent at a time; a bad one keeps that agent's stored record.
func (p *parsed) agentRecords(raw json.RawMessage) map[string]vault.AgentRecord {
	if raw == nil {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		p.skip("agents."+recordsKey, err)
		return nil
	}
	recs := make(map[string]vault.AgentRecord, len(entries))
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		rec, err := decodeAgentRecord(name, entries[name])
		if err != nil {
			p.skip("agents."+recordsKey+"."+name, err)
			continue
		}
		recs[name] = rec
	}
	return recs
}

// refuseInvalidRecords drops each changed record agent.ValidateRecord refuses, warning by path; stored ones pass.
func (p *parsed) refuseInvalidRecords(stored map[string]vault.AgentRecord) {
	if p.recs == nil {
		return
	}
	valid := make(map[string]vault.AgentRecord, len(p.recs))
	for _, name := range slices.Sorted(maps.Keys(p.recs)) {
		rec := p.recs[name]
		if old, ok := stored[name]; !ok || !reflect.DeepEqual(old, rec) {
			if err := agent.ValidateRecord(name, rec); err != nil {
				p.skip("agents."+recordsKey+"."+name, err)
				continue
			}
		}
		valid[name] = rec
	}
	p.recs = valid
}

// keptRecords is p's records with the stored record restored for every agent (or the whole map) p keeps.
func keptRecords(p *parsed, cur map[string]vault.AgentRecord) map[string]vault.AgentRecord {
	if p.keep["agents."+recordsKey] {
		return cur
	}
	next := maps.Clone(p.recs)
	for name, rec := range cur {
		if p.keep["agents."+recordsKey+"."+name] {
			if next == nil {
				next = map[string]vault.AgentRecord{}
			}
			next[name] = rec
		}
	}
	return next
}

// applyAgentRecords stores next when it differs from cur.
func applyAgentRecords(t *vault.Tx, cur, next map[string]vault.AgentRecord) error {
	if len(cur) == 0 && len(next) == 0 || reflect.DeepEqual(cur, next) {
		return nil
	}
	return t.PutAgentRecords(next)
}
