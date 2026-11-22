package vault

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/route"
)

// AgentRecordsKey is the Overlay entry holding every agent's record: a JSON object of agent name -> AgentRecord.
const AgentRecordsKey = "agent-records"

// PoolKey is the Overlay entry holding the discovery pool: a JSON array of Databricks wire ids.
const PoolKey = "discovery-models"

// AgentRecord is one agent's vault record; every field is optional and a zero record is not stored.
type AgentRecord struct {
	Model         string      `json:"model,omitempty"`
	Effort        string      `json:"effort,omitempty"`
	Context       string      `json:"context,omitempty"` // copilot only today
	Route         route.Route `json:"route,omitzero"`
	FallbackRoute route.Route `json:"fallbackRoute,omitzero"`
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
	if !r.Route.IsZero() || !r.FallbackRoute.IsZero() || r.Permissions != nil || r.Launch != nil {
		return false
	}
	if len(r.Aliases) != 0 || len(r.Extras) != 0 {
		return false
	}
	return r.Model+r.Effort+r.Context == ""
}

// EffectiveRoute is the stored dispatch route.
func (r AgentRecord) EffectiveRoute() route.Route { return r.Route }

// EffectiveFallbackRoute is the stored fallback route.
func (r AgentRecord) EffectiveFallbackRoute() route.Route { return r.FallbackRoute }

func legacyRoute(profile string) route.Route {
	switch profile {
	case "":
		return route.Route{}
	case "databricks":
		return route.Route{Provider: "databricks", Profile: "auto"}
	}
	return route.Route{Profile: profile}
}

// WithPin returns r with the pin set to val; ACTIVE=off clears the route, any other ACTIVE is a no-op.
func (r AgentRecord) WithPin(field, val string) AgentRecord {
	val = strings.TrimSpace(val)
	switch field {
	case "PROFILE":
		r.Route = legacyRoute(val)
	case "MODEL":
		r.Model = val
	case "EFFORT":
		r.Effort = val
	case "CONTEXT":
		r.Context = val
	case "GITHUB_USER":
		r.Route = route.Route{}
		if val != "" {
			r.Route = route.Route{Provider: "github", Account: val}
		}
	case "ACTIVE":
		if val == "off" {
			r.Route = route.Route{}
		}
	default:
		r = r.WithAlias(strings.ToLower(strings.TrimSuffix(field, "_MODEL")), val)
	}
	return r
}

// WithExtra returns a copy with extras key set to val; a stored "" unsets a built-in (sandboxMode), so it is kept.
func (r AgentRecord) WithExtra(key, val string) AgentRecord {
	extras := maps.Clone(r.Extras)
	if extras == nil {
		extras = map[string]string{}
	}
	extras[key] = val
	r.Extras = extras
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
		t.nameGithubProfiles(out)
	}
	return out, nil
}

func (t *Tx) nameGithubProfiles(recs map[string]AgentRecord) {
	renames := t.githubRowRenames()
	spec := func(s string) string {
		if head, rest, ok := strings.Cut(s, ":"); ok && renames[head] != "" {
			return renames[head] + ":" + rest
		}
		return s
	}
	for n, r := range recs {
		r.Route.Profile = cmp.Or(renames[r.Route.Profile], r.Route.Profile)
		r.FallbackRoute.Profile = cmp.Or(renames[r.FallbackRoute.Profile], r.FallbackRoute.Profile)
		r.Model = spec(r.Model)
		if r.Aliases != nil {
			r.Aliases = maps.Clone(r.Aliases)
			for a, v := range r.Aliases {
				r.Aliases[a] = spec(v)
			}
		}
		recs[n] = r
	}
}

func (t *Tx) githubRowRenames() map[string]string {
	var doc struct {
		Providers map[string]map[string]struct {
			Login string `json:"login"`
		} `json:"providers"`
	}
	if raw, ok := t.Get(Overlay, "providers"); ok {
		_ = json.Unmarshal([]byte(raw), &doc)
	}
	logins, names := map[string]string{}, []string{}
	for prov, rows := range doc.Providers {
		for key, row := range rows {
			names = append(names, key)
			if prov == "github" && row.Login != "" {
				logins[key] = row.Login
			}
		}
	}
	renames, _ := LoginRenames(logins, names)
	return renames
}

// LoginRenames maps github row keys to logins; a login already taken (case-insensitive) keeps its key with a warning.
func LoginRenames(logins map[string]string, names []string) (map[string]string, []string) {
	claimed := map[string]bool{}
	for _, n := range names {
		claimed[strings.ToLower(n)] = true
	}
	renames := map[string]string{}
	var warns []string
	for _, key := range slices.Sorted(maps.Keys(logins)) {
		login := logins[key]
		if strings.EqualFold(login, key) {
			continue
		}
		if k := strings.ToLower(login); claimed[k] {
			warns = append(warns, fmt.Sprintf("github row %q keeps its name: login %q is already the name or login of another row", key, login))
		} else {
			claimed[k] = true
			renames[key] = login
		}
	}
	return renames, warns
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

func (t *Tx) migrateAgentRecords() error {
	raw, ok := t.Get(Overlay, AgentRecordsKey)
	if !ok {
		return nil
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return fmt.Errorf("vault: migrate %s: %w", AgentRecordsKey, err)
	}
	for name, raw := range records {
		converted, err := migrateAgentRecord(raw)
		if err != nil {
			return fmt.Errorf("vault: migrate agent %q: %w", name, err)
		}
		records[name] = converted
	}
	b, err := json.Marshal(records)
	if err != nil {
		return err
	}
	return t.Put(Overlay, AgentRecordsKey, string(b))
}

func migrateAgentRecord(raw json.RawMessage) (json.RawMessage, error) {
	var old struct {
		Profile, Account, Fallback string
		Route, FallbackRoute       route.Route
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return raw, nil
	}
	lead, fallback := legacyRoute(old.Profile), legacyRoute(old.Fallback)
	if old.Account != "" {
		lead = route.Route{Provider: "github", Account: old.Account}
		if old.Fallback == "" && old.Route.IsZero() {
			fallback = legacyRoute(old.Profile)
		}
	}
	if err := migrateRouteField(fields, "route", old.Route, lead); err != nil {
		return nil, err
	}
	if err := migrateRouteField(fields, "fallbackRoute", old.FallbackRoute, fallback); err != nil {
		return nil, err
	}
	delete(fields, "profile")
	delete(fields, "account")
	delete(fields, "fallback")
	return json.Marshal(fields)
}

func migrateRouteField(fields map[string]json.RawMessage, key string, stored, legacy route.Route) error {
	if !stored.IsZero() || legacy.IsZero() {
		return nil
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		return err
	}
	fields[key] = b
	return nil
}

// PoolOverlay is the stored discovery pool JSON.
func (t *Tx) PoolOverlay() (string, bool) { return t.Get(Overlay, PoolKey) }

// OwnLoginProfiles are each external agent's own-login profile; a test pins them to defaults.json and the roster.
var OwnLoginProfiles = map[string]string{
	agentid.Codex: "chatgpt", agentid.Copilot: "copilot", agentid.Gemini: "antigravity", agentid.OpenCode: "zen-free",
}

// agentRecordCache holds the decoded records while the vault file's stamp holds; a write changes the stamp.
var agentRecordCache = paths.NewStampCache[map[string]AgentRecord]("vault.agentrecord")

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
