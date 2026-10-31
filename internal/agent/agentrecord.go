package agent

import (
	"maps"

	"github.com/edotau/claude-code/internal/vault"
)

// AgentRecord is one agent's vault record (profile, model, effort, context, account; claude adds tier Slots).
type AgentRecord = vault.AgentRecord

// AgentRecords reads the vault once; every roster agent is present (zero value when unset), a read error is empty.
func AgentRecords() map[string]AgentRecord {
	var recs map[string]AgentRecord
	_ = vault.View(func(tx *vault.Tx) (err error) { recs, err = tx.AgentRecords(); return err })
	out := make(map[string]AgentRecord, len(rosterRows))
	for _, k := range rosterRows {
		out[k.Name] = recs[k.Name]
	}
	return out
}

// GetAgentRecord is agent's record (zero value when unset).
func GetAgentRecord(agent string) AgentRecord { return AgentRecords()[agent] }

// SetAgentRecord replaces agent's whole record in one vault transaction.
func SetAgentRecord(agent string, r AgentRecord) error {
	return UpdateAgentRecords(func(m map[string]AgentRecord) { m[agent] = r })
}

// UpdateAgentRecords applies fn to the stored records and writes them back in one vault transaction.
func UpdateAgentRecords(fn func(map[string]AgentRecord)) error {
	return vault.Update(func(tx *vault.Tx) error {
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		next := maps.Clone(recs)
		fn(next)
		return tx.PutAgentRecords(next)
	})
}
