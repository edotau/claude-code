package vault

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
)

// AgentRecordsKey is the Overlay entry holding every agent's record: a JSON object of agent name -> AgentRecord.
const AgentRecordsKey = "agent-records"

// PoolKey is the Overlay entry holding the discovery pool: a JSON array of Databricks wire ids.
const PoolKey = "discovery-models"

// AgentRecord is one agent's vault record; every field is optional and a zero record is not stored.
type AgentRecord struct {
	Profile string `json:"profile,omitempty"` // "" = off (claude: "" = automatic pick, always on)
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Context string `json:"context,omitempty"` // copilot only today
	Account string `json:"account,omitempty"` // copilot GitHub user
	// Fallback is the one profile the agent falls back to when its Profile is unavailable.
	Fallback string `json:"fallback,omitempty"`
	// Aliases are claude's per-alias model pins (CLAUDE_<ALIAS>_MODEL), keyed by lower-case alias.
	Aliases map[string]string `json:"aliases,omitempty"`
	// Permissions is the default dispatch policy in Claude Code rule grammar; nil inherits the built-in.
	Permissions *AgentPolicy `json:"permissions,omitempty"`
	// Launch is env and argv every launch carries; nil inherits the built-in, non-nil replaces it whole.
	Launch *AgentLaunch `json:"launch,omitempty"`
	// Extras are vendor-native knobs, merged key by key over the built-in; "" unsets a built-in key.
	Extras map[string]string `json:"extras,omitempty"`
}

// AgentPolicy is a dispatch policy: mode "", default, auto or plan, and rule strings such as "Bash(rm:*)".
type AgentPolicy struct {
	Mode  string   `json:"mode,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// AgentLaunch is env (KEY=VALUE) for every launch and args for interactive launches only.
type AgentLaunch struct {
	Env  []string `json:"env,omitempty"`
	Args []string `json:"args,omitempty"`
}

// IsZero reports a record with nothing set.
func (r AgentRecord) IsZero() bool {
	return r.Profile == "" && r.Model == "" && r.Effort == "" && r.Context == "" && r.Account == "" && r.Fallback == "" && len(r.Aliases) == 0 &&
		r.Permissions == nil && r.Launch == nil && len(r.Extras) == 0
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
		r = r.WithAlias(strings.ToLower(strings.TrimSuffix(field, "_MODEL")), val)
	}
	return r
}

// WithAlias returns r with the model alias set to val ("" removes it), leaving r's Aliases map untouched.
func (r AgentRecord) WithAlias(alias, val string) AgentRecord {
	aliases := maps.Clone(r.Aliases)
	if aliases == nil {
		aliases = map[string]string{}
	}
	if aliases[alias] = val; val == "" {
		delete(aliases, alias)
	}
	r.Aliases = aliases
	return r
}

// AgentRecords decodes the stored records; absent is empty.
func (t *Tx) AgentRecords() (map[string]AgentRecord, error) {
	out := map[string]AgentRecord{}
	if raw, ok := t.Get(Overlay, AgentRecordsKey); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, fmt.Errorf("vault: %s is corrupt: %w", AgentRecordsKey, err)
		}
		if out == nil { // a stored JSON null decodes the map to nil; writes into it would panic
			out = map[string]AgentRecord{}
		}
	}
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

// PoolOverlay is the stored discovery pool JSON.
func (t *Tx) PoolOverlay() (string, bool) { return t.Get(Overlay, PoolKey) }

// OwnLoginProfiles are each external agent's own-login profile; a test pins them to defaults.json and the roster.
var OwnLoginProfiles = map[string]string{
	agentid.Codex: "chatgpt", agentid.Copilot: "personal", agentid.Gemini: "antigravity", agentid.OpenCode: "zen-free",
}

// agentRecordCache holds the decoded records while the vault file's stamp holds; a write changes the stamp.
var agentRecordCache paths.StampCache[map[string]AgentRecord]

// loadAgentRecords is the one vault open behind ReadAgentRecords; a var so tests count the opens.
var loadAgentRecords = func() (recs map[string]AgentRecord, err error) {
	err = View(func(tx *Tx) (err error) { recs, err = tx.AgentRecords(); return err })
	return recs, err
}

// ReadAgentRecords reads the vault once; every name is present (zero value when unset), and a read error is returned.
func ReadAgentRecords(names []string) (map[string]AgentRecord, error) {
	recs, err := agentRecordCache.Get(Path(), loadAgentRecords)
	out := make(map[string]AgentRecord, len(names))
	for _, n := range names {
		out[n] = recs[n].Clone()
	}
	return out, err
}

// Clone is a deep copy of r, so a caller's edit never reaches the cached or stored record.
func (r AgentRecord) Clone() AgentRecord {
	r.Aliases, r.Extras = maps.Clone(r.Aliases), maps.Clone(r.Extras)
	if r.Permissions != nil {
		p := *r.Permissions
		p.Allow, p.Ask, p.Deny = slices.Clone(p.Allow), slices.Clone(p.Ask), slices.Clone(p.Deny)
		r.Permissions = &p
	}
	if r.Launch != nil {
		l := *r.Launch
		l.Env, l.Args = slices.Clone(l.Env), slices.Clone(l.Args)
		r.Launch = &l
	}
	return r
}

// AgentRecords is ReadAgentRecords for display callers: a read error reads as every name unset.
func AgentRecords(names []string) map[string]AgentRecord {
	recs, _ := ReadAgentRecords(names)
	return recs
}

// GetAgentRecord is agent's record (zero value when unset or unreadable).
func GetAgentRecord(agent string) AgentRecord { return AgentRecords([]string{agent})[agent] }

// SetAgentRecord replaces agent's whole record in one vault transaction.
func SetAgentRecord(agent string, r AgentRecord) error {
	return UpdateAgentRecords(func(m map[string]AgentRecord) { m[agent] = r })
}

// UpdateAgentRecords applies fn to the stored records and writes them back in one vault transaction.
func UpdateAgentRecords(fn func(map[string]AgentRecord)) error {
	return Update(func(tx *Tx) error {
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		fn(recs)
		return tx.PutAgentRecords(recs)
	})
}
