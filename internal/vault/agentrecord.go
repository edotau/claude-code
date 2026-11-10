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

// agentRecordSchema is the vault layout that folded the <AGENT>_<FIELD> pins into records.
const agentRecordSchema = 4

// ultraSchema is the layout that renamed the claude.ai subscription profile "max" to "ultra".
const ultraSchema = 7

// Pre-schema-6 literals of the router's curated model pool; the one migration below is their only reader.
const (
	oldPoolKey   = "byok-models"
	oldPoolModel = "byok"
	// PoolKey is the Overlay entry holding the discovery pool: a JSON array of Databricks wire ids.
	PoolKey = "discovery-models"
)

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

// LegacyAgents are the roster agents whose <AGENT>_<FIELD> pins fold into records; claude is first.
var LegacyAgents = agentid.All()

var legacyAliases = []string{"opus", "sonnet", "haiku", "fable"}

// parseAgentPin splits a legacy pin key into agent and field (PROFILE, MODEL, ..., claude's <TIER>_MODEL); ok=false else.
func parseAgentPin(key string) (agent, field string, ok bool) {
	for _, a := range LegacyAgents {
		f, found := strings.CutPrefix(key, strings.ToUpper(a)+"_")
		if !found {
			continue
		}
		switch {
		case slices.Contains([]string{"PROFILE", "MODEL", "EFFORT", "CONTEXT"}, f),
			f == "ACTIVE" && a != agentid.Claude,
			f == "GITHUB_USER" && a == agentid.Copilot:
			return a, f, true
		case a == agentid.Claude && strings.HasSuffix(f, "_MODEL") && slices.Contains(legacyAliases, strings.ToLower(strings.TrimSuffix(f, "_MODEL"))):
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
		alias := strings.ToLower(strings.TrimSuffix(field, "_MODEL"))
		aliases := maps.Clone(r.Aliases)
		if aliases == nil {
			aliases = map[string]string{}
		}
		if aliases[alias] = val; val == "" {
			delete(aliases, alias)
		}
		r.Aliases = aliases
	}
	return r
}

// AgentRecords decodes the stored records with any not-yet-migrated legacy pins folded in; absent is empty.
func (t *Tx) AgentRecords() (map[string]AgentRecord, error) {
	out := map[string]AgentRecord{}
	if raw, ok := t.Get(Overlay, AgentRecordsKey); ok && raw != "" {
		if err := json.Unmarshal([]byte(renameSlotsKey(raw)), &out); err != nil {
			return nil, fmt.Errorf("vault: %s is corrupt: %w", AgentRecordsKey, err)
		}
		if out == nil { // a stored JSON null decodes the map to nil; writes into it would panic
			out = map[string]AgentRecord{}
		}
	}
	foldAgentPins(out, t.All(Pins)) // a read before the schema-4 migration sees the old pins, not "every agent off"
	for n, r := range out {         // a read before the schema-6 migration sees "byok" as "discovery"
		if r.Model == oldPoolModel {
			r.Model = "discovery"
			out[n] = r
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

// renameSlotsKey rewrites each record's pre-schema-5 "slots" key to "aliases" so an unmigrated read still sees them.
func renameSlotsKey(raw string) string {
	if !strings.Contains(raw, `"slots"`) {
		return raw
	}
	var recs map[string]map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &recs) != nil {
		return raw
	}
	for _, r := range recs {
		if v, ok := r["slots"]; ok {
			if _, has := r["aliases"]; !has {
				r["aliases"] = v
			}
			delete(r, "slots")
		}
	}
	b, err := json.Marshal(recs)
	if err != nil {
		return raw
	}
	return string(b)
}

// migrateSlotsToAliases rewrites every stored record under the "aliases" key (AgentRecords reads the old "slots").
func migrateSlotsToAliases(t *Tx) error {
	recs, err := t.AgentRecords()
	if err != nil {
		return err
	}
	return t.PutAgentRecords(recs)
}

// migrateByokToDiscovery moves the "byok-models" overlay value to "discovery-models" and the "byok" model pins to
// "discovery"; a value already under the new key wins, and a vault with neither is untouched.
func migrateByokToDiscovery(t *Tx) error {
	if old, ok := t.Get(Overlay, oldPoolKey); ok {
		if cur, has := t.Get(Overlay, PoolKey); !has || cur == "" {
			if err := t.Put(Overlay, PoolKey, old); err != nil {
				return err
			}
		}
		if err := t.Put(Overlay, oldPoolKey, ""); err != nil {
			return err
		}
	}
	return nil // the "byok" model pins are stored by migrateSlotsToAliases, whose AgentRecords read maps them
}

// migrateMaxToUltra renames the "max" profile in agent records and the providers overlay (default, fallback, row).
func migrateMaxToUltra(t *Tx) error {
	recs, err := t.AgentRecords()
	if err != nil {
		return err
	}
	for n, r := range recs {
		r.Profile, r.Fallback = maxToUltra(r.Profile), maxToUltra(r.Fallback)
		if alias, ok := strings.CutPrefix(r.Model, "max:"); ok {
			r.Model = "ultra:" + alias
		}
		recs[n] = r
	}
	if err := t.PutAgentRecords(recs); err != nil {
		return err
	}
	raw, ok := t.Get(Overlay, "providers")
	if !ok || raw == "" {
		return nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.ReplaceAll(raw, `"anthropic·max"`, `"anthropic·ultra"`)), &doc) != nil {
		return nil // a malformed overlay is left for providers.Load to report
	}
	var def string
	var fallback []string
	var rows map[string]map[string]json.RawMessage
	_ = json.Unmarshal(doc["default"], &def)
	_ = json.Unmarshal(doc["fallback"], &fallback)
	_ = json.Unmarshal(doc["providers"], &rows)
	if def == "max" {
		doc["default"], _ = json.Marshal("ultra")
	}
	if i := slices.Index(fallback, "max"); i >= 0 {
		fallback[i] = "ultra"
		doc["fallback"], _ = json.Marshal(fallback)
	}
	if row, ok := rows["anthropic"]["max"]; ok {
		delete(rows["anthropic"], "max")
		rows["anthropic"]["ultra"] = row
		doc["providers"], _ = json.Marshal(rows)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return t.Put(Overlay, "providers", string(b))
}

func maxToUltra(p string) string {
	if p == "max" {
		return "ultra"
	}
	return p
}

// PoolOverlay is the stored discovery pool JSON, reading a pre-schema-6 "byok-models" entry until the migration moves it.
func (t *Tx) PoolOverlay() (string, bool) {
	if raw, ok := t.Get(Overlay, PoolKey); ok && raw != "" {
		return raw, true
	}
	return t.Get(Overlay, oldPoolKey)
}

// foldAgentPins applies every legacy pin in pins to recs (ACTIVE last, so off clears the profile) and lists the keys.
func foldAgentPins(recs map[string]AgentRecord, pins map[string]string) (folded []string) {
	keys := slices.Sorted(maps.Keys(pins))
	for _, active := range []bool{false, true} {
		for _, k := range keys {
			a, f, ok := parseAgentPin(k)
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
		if _, f, ok := parseAgentPin(k); ok && f == "ACTIVE" {
			folded = append(folded, k)
		}
	}
	keepOwnLogin(recs, pins)
	return folded
}

// OwnLoginProfiles are each external agent's own-login profile; a test pins them to defaults.json and the roster.
var OwnLoginProfiles = map[string]string{
	agentid.Codex: "chatgpt", agentid.Copilot: "personal", agentid.Gemini: "antigravity", agentid.OpenCode: "zen-free",
}

// keepOwnLogin gives each profile-less agent that was not ACTIVE=off its own-login profile (the old unset = on);
// it runs only while a legacy pin key remains, so once the migration deletes them a cleared profile stays cleared.
func keepOwnLogin(recs map[string]AgentRecord, pins map[string]string) {
	legacy := false
	for k := range pins {
		if _, _, ok := parseAgentPin(k); ok {
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
		out[n] = recs[n].clone()
	}
	return out, err
}

// clone is a deep copy of r, so a caller's edit never reaches the cached record.
func (r AgentRecord) clone() AgentRecord {
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
		next := maps.Clone(recs)
		fn(next)
		return tx.PutAgentRecords(next)
	})
}
