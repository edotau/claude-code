package vault

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// AgentRecordsKey is the Overlay entry holding every agent's record: a JSON object of agent name -> AgentRecord.
const AgentRecordsKey = "agent-records"

// agentRecordSchema is the vault layout that folded the <AGENT>_<FIELD> pins into records.
const agentRecordSchema = 4

// AgentRecord is one agent's vault record; every field is optional and a zero record is not stored.
type AgentRecord struct {
	Profile string `json:"profile,omitempty"` // "" = off (claude: "" = automatic pick, always on)
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Context string `json:"context,omitempty"` // copilot only today
	Account string `json:"account,omitempty"` // copilot GitHub user
	// Slots are claude's per-tier model pins (CLAUDE_<TIER>_MODEL), keyed by lower-case tier.
	Slots map[string]string `json:"slots,omitempty"`
}

// IsZero reports a record with nothing set.
func (r AgentRecord) IsZero() bool {
	return r.Profile == "" && r.Model == "" && r.Effort == "" && r.Context == "" && r.Account == "" && len(r.Slots) == 0
}

// LegacyAgents are the roster agents whose <AGENT>_<FIELD> pins fold into records; claude is first.
var LegacyAgents = []string{"claude", "codex", "copilot", "gemini", "opencode"}

var legacyTiers = []string{"opus", "sonnet", "haiku", "fable"}

// ParseAgentPin splits a legacy pin key into agent and field (PROFILE, MODEL, ..., claude's <TIER>_MODEL); ok=false else.
func ParseAgentPin(key string) (agent, field string, ok bool) {
	for _, a := range LegacyAgents {
		f, found := strings.CutPrefix(key, strings.ToUpper(a)+"_")
		if !found {
			continue
		}
		switch {
		case slices.Contains([]string{"PROFILE", "MODEL", "EFFORT", "CONTEXT"}, f),
			f == "ACTIVE" && a != "claude",
			f == "GITHUB_USER" && a == "copilot":
			return a, f, true
		case a == "claude" && strings.HasSuffix(f, "_MODEL") && slices.Contains(legacyTiers, strings.ToLower(strings.TrimSuffix(f, "_MODEL"))):
			return a, f, true
		}
	}
	return "", "", false
}

// WithPin returns r with the legacy pin field set to val; ACTIVE=off clears the profile, any other ACTIVE is a no-op.
func (r AgentRecord) WithPin(field, val string) AgentRecord {
	val = strings.TrimSpace(val)
	switch field {
	case "PROFILE":
		r.Profile = val
	case "MODEL":
		r.Model = val
	case "EFFORT":
		r.Effort = val
	case "CONTEXT":
		r.Context = val
	case "GITHUB_USER":
		r.Account = val
	case "ACTIVE":
		if val == "off" {
			r.Profile = ""
		}
	default:
		slot := strings.ToLower(strings.TrimSuffix(field, "_MODEL"))
		slots := maps.Clone(r.Slots)
		if slots == nil {
			slots = map[string]string{}
		}
		if slots[slot] = val; val == "" {
			delete(slots, slot)
		}
		r.Slots = slots
	}
	return r
}

// AgentRecords decodes the stored records with any not-yet-migrated legacy pins folded in; absent is empty.
func (t *Tx) AgentRecords() (map[string]AgentRecord, error) {
	out := map[string]AgentRecord{}
	if raw, ok := t.Get(Overlay, AgentRecordsKey); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, fmt.Errorf("vault: %s is corrupt: %w", AgentRecordsKey, err)
		}
	}
	foldAgentPins(out, t.All(Pins)) // a read before the schema-4 migration sees the old pins, not "every agent off"
	return out, nil
}

// PutAgentRecords stores recs, dropping zero records; an empty map removes the entry.
func (t *Tx) PutAgentRecords(recs map[string]AgentRecord) error {
	kept := map[string]AgentRecord{}
	for n, r := range recs {
		if !r.IsZero() {
			kept[n] = r
		}
	}
	if len(kept) == 0 {
		return t.Put(Overlay, AgentRecordsKey, "")
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return t.Put(Overlay, AgentRecordsKey, string(b))
}

// LegacyPins renders recs as the pin keys they replace; active adds <AGENT>_ACTIVE=off for each profile-less agent.
func LegacyPins(recs map[string]AgentRecord, active bool) map[string]string {
	out := map[string]string{}
	set := func(a, f, v string) {
		if v != "" {
			out[strings.ToUpper(a)+"_"+f] = v
		}
	}
	for _, a := range LegacyAgents {
		r := recs[a]
		set(a, "PROFILE", r.Profile)
		set(a, "MODEL", r.Model)
		set(a, "EFFORT", r.Effort)
		set(a, "CONTEXT", r.Context)
		set(a, "GITHUB_USER", r.Account)
		for slot, v := range r.Slots {
			set(a, strings.ToUpper(slot)+"_MODEL", v)
		}
		if active && a != "claude" && r.Profile == "" {
			out[strings.ToUpper(a)+"_ACTIVE"] = "off"
		}
	}
	return out
}

// foldAgentPins applies every legacy pin in pins to recs (ACTIVE last, so off clears the profile) and lists the keys.
func foldAgentPins(recs map[string]AgentRecord, pins map[string]string) (folded []string) {
	keys := slices.Sorted(maps.Keys(pins))
	for _, active := range []bool{false, true} {
		for _, k := range keys {
			a, f, ok := ParseAgentPin(k)
			if !ok || (f == "ACTIVE") != active {
				continue
			}
			recs[a] = recs[a].WithPin(f, pins[k])
			if !active {
				folded = append(folded, k)
			}
		}
	}
	for _, k := range keys {
		if _, f, ok := ParseAgentPin(k); ok && f == "ACTIVE" {
			folded = append(folded, k)
		}
	}
	keepOwnLogin(recs, pins)
	return folded
}

// OwnLoginProfiles are each external agent's own-login profile; a test pins them to defaults.json and the roster.
var OwnLoginProfiles = map[string]string{"codex": "chatgpt", "copilot": "personal", "gemini": "antigravity", "opencode": "zen-free"}

// keepOwnLogin gives each profile-less agent that was not ACTIVE=off its own-login profile (the old unset = on);
// it runs only while a legacy pin key remains, so once the migration deletes them a cleared profile stays cleared.
func keepOwnLogin(recs map[string]AgentRecord, pins map[string]string) {
	legacy := false
	for k := range pins {
		if _, _, ok := ParseAgentPin(k); ok {
			legacy = true
			break
		}
	}
	if !legacy {
		return
	}
	for _, a := range LegacyAgents[1:] {
		if r := recs[a]; r.Profile == "" && pins[strings.ToUpper(a)+"_ACTIVE"] != "off" {
			r.Profile = OwnLoginProfiles[a]
			recs[a] = r
		}
	}
}

// migrateAgentPins folds every legacy pin into the records and deletes the keys; it returns the agents left off.
func migrateAgentPins(t *Tx) (off []string, err error) {
	recs, err := t.AgentRecords()
	if err != nil {
		return nil, err
	}
	for _, k := range foldAgentPins(recs, t.All(Pins)) {
		if err := t.Put(Pins, k, ""); err != nil {
			return nil, err
		}
	}
	if err := t.PutAgentRecords(recs); err != nil {
		return nil, err
	}
	for _, a := range LegacyAgents[1:] {
		if recs[a].Profile == "" {
			off = append(off, a)
		}
	}
	return off, nil
}
