package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// IsGithubProfile is true for a registry row whose models come from the Copilot catalog, not a gateway listing.
func IsGithubProfile(p *providers.Provider) bool { return p != nil && p.Provider == "github" }

// ProfileCatalogSupport requires an explicit account for discovery without launch-account fallback.
func ProfileCatalogSupport(p *providers.Provider) string {
	if strings.TrimSpace(p.Login) == "" {
		return "Copilot discovery requires an explicit profile login; implicit launch-account fallback is disabled"
	}
	return ""
}

// discoverySource is where p's models are discovered from and, when discovery cannot run, why not.
func discoverySource(p *providers.Provider) (source, skip string) {
	source, skip = providers.DiscoverySource(p)
	if IsGithubProfile(p) {
		skip = ProfileCatalogSupport(p)
	}
	if source == "" && skip != "" {
		skip += "; choose its models in the vendor CLI itself"
	}
	return source, skip
}

// githubLister lists a github row's live models; the copilot package supplies it.
type githubLister func(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error)

// discoverGateway is the gateway listing call; tests swap it for a canned listing.
var discoverGateway = providers.DiscoverModels

// DiscoverResult is one profile's `models discover` outcome.
type DiscoverResult struct {
	Provider     string                `json:"provider"`
	Profile      string                `json:"profile"`
	Source       string                `json:"source"`
	Discovered   int                   `json:"discovered"`
	Additions    []providers.ModelInfo `json:"additions"`
	Dropped      []string              `json:"skipped_dropped,omitempty"`
	Deprecated   []string              `json:"dropped_deprecated,omitempty"`
	ProbeNote    string                `json:"probe_note,omitempty"`
	Destinations []string              `json:"destinations"`
	Status       string                `json:"status"`
	Note         string                `json:"note,omitempty"`
}

// missingKeyNote explains a key profile whose key is absent or unreadable ("" when it resolves or is no key row).
func missingKeyNote(ctx context.Context, p *providers.Provider) string {
	if p.Auth.Type != providers.AuthKey {
		return ""
	}
	if _, err := providers.Credential(ctx, p); err == nil {
		return ""
	}
	name := cmp.Or(providers.SecretName(p.Provider, p.Name, providers.AuthKey), p.Auth.Env)
	if _, err := vault.HasSecretErr(name); err != nil || providers.CredentialPresent(p) {
		return "this profile's API key could not be read; check the vault with `claude-code doctor`"
	}
	return "no API key stored for this profile; add one with `claude-code vault set " + name + "`"
}

// discoverUsable is p's live listing, or a skip note when p has no API key or its account lists nothing.
func discoverUsable(ctx context.Context, p *providers.Provider, github githubLister) (live []providers.ModelInfo, note string, err error) {
	if note = missingKeyNote(ctx, p); note != "" {
		return nil, note, nil
	}
	list := discoverGateway
	if IsGithubProfile(p) {
		list = github
	}
	if live, err = list(ctx, p); err == nil && len(live) == 0 {
		note = "the catalog listed no models for this account; " + emptyCatalogStep(p)
	}
	return live, note, err
}

// emptyCatalogStep is what to check when p's live catalog lists nothing.
func emptyCatalogStep(p *providers.Provider) string {
	if IsGithubProfile(p) {
		return "check `gh auth status` and that " + cmp.Or(p.Login, "the account") + " holds a Copilot seat"
	}
	return "check the profile's credential with `claude-code doctor`"
}

// discoverPickerProfile live-discovers p's models and previews (or, with apply, appends) the saved picker additions.
func discoverPickerProfile(p *providers.Provider, github githubLister, apply, verify bool) DiscoverResult {
	r := DiscoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := discoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, note, err := discoverUsable(ctx, p, github)
	if err != nil {
		r.Status = "error"
		r.Note = err.Error() + " (no fallback saved)"
		return r
	}
	if note != "" {
		r.Note = note
		return r
	}
	r.Discovered = len(live)
	r.Destinations = discoverDestinations(p)
	if r.Dropped, err = droppedLive(p, live); err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	r.Dropped = withReasons(p, r.Dropped)
	r.Deprecated, r.ProbeNote = databricksListing(ctx, p, live, apply, verify)
	var added []providers.ModelInfo
	if apply {
		added, err = AppendPickerModels(p, live)
	} else {
		added, err = previewPickerModels(p, live)
	}
	if err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	if len(added) > 0 {
		r.Additions = added
	}
	r.Status = "preview"
	if apply {
		r.Status = "applied"
	}
	r.Note = "Listing is not inference proof; existing selections and native vendor catalogs are unchanged."
	return r
}

func discoverDestinations(p *providers.Provider) []string {
	if p.Provider == "github" {
		return []string{"sealed profile history only (Copilot pickers retain live policy filtering)"}
	}
	d := []string{"configure model picker", "agent model picker", "OpenCode harness-generated catalog (next routed launch)"}
	if p.Preset == providers.PresetDatabricks {
		d = append(d, "Codex generated catalog (Responses models with bundled metadata only)")
	}
	return d
}

// DiscoverPickerProfiles runs discoverPickerProfile over names, listing github rows with github; ok is false when one errored or none completed.
func DiscoverPickerProfiles(reg *providers.Registry, names []string, github githubLister, apply, verify bool) (results []DiscoverResult, ok bool) {
	results = make([]DiscoverResult, 0, len(names))
	ok = true
	completed := 0
	for _, name := range names {
		p, err := reg.Get(name)
		if err != nil {
			ok = false
			results = append(results, DiscoverResult{Profile: name, Additions: []providers.ModelInfo{}, Destinations: []string{},
				Status: "error", Note: err.Error()})
			continue
		}
		r := discoverPickerProfile(p, github, apply, verify)
		if r.Status == "error" {
			ok = false
		}
		if r.Status == "applied" || r.Status == "preview" {
			completed++
		}
		results = append(results, r)
	}
	return results, ok && completed > 0
}

// DropResult is one profile's `models discover --drop|--undrop` outcome; Profile "" is the global saved menu.
type DropResult struct {
	Profile string   `json:"profile"`
	Removed []string `json:"removed"`
	Error   string   `json:"error,omitempty"`
}

// DropPickerProfiles drops (or un-drops) ids from the global menu (when global) and each named profile's picker.
// write false previews. ok is false when any profile errored.
func DropPickerProfiles(reg *providers.Registry, names, ids []string, undrop, global, write bool) (out []DropResult, ok bool) {
	ok = true
	if global && !undrop {
		r := DropResult{Removed: []string{}}
		removed, err := providers.DropFromMenu(ids, write)
		r.Removed = append(r.Removed, removed...)
		if err != nil {
			r.Error, ok = err.Error(), false
		}
		out = append(out, r)
	}
	for _, name := range names {
		r := DropResult{Profile: name, Removed: []string{}}
		p, err := reg.Get(name)
		if err == nil {
			var removed []string
			removed, err = EditPickerDrops(p, ids, undrop, write)
			r.Removed = append(r.Removed, removed...)
		}
		if err != nil {
			r.Error, ok = err.Error(), false
		}
		out = append(out, r)
	}
	return out, ok
}

// probeEndpoints is the deprecation probe's HTTP call; tests swap it for canned replies.
var probeEndpoints = serving.Probe

// databricksListing keeps an applied listing's api_types and limits, then drops the ids the probe finds unserved.
func databricksListing(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, apply, verify bool) (dropped []string, note string) {
	if p.Preset != providers.PresetDatabricks {
		return nil, ""
	}
	if !apply {
		return nil, "no deprecation probe sent in a preview; --apply probes new ids (--verify also the saved ones)"
	}
	if err := serving.WarmServed(p.Name, live, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s served listing not saved: %v\n", p.Name, err)
	}
	ids, err := probeCandidates(p, live, verify)
	if err != nil {
		return nil, "deprecation probe skipped: " + err.Error()
	}
	unserved, broken, err := unservedIDs(ctx, p, live, ids)
	for i := 0; err == nil && i < len(unserved); i++ {
		_, err = RecordDropped(p, unserved[i].ID, unserved[i].Reason, time.Now())
	}
	if err != nil {
		return nil, "deprecation probe changed nothing: " + err.Error()
	}
	for _, u := range unserved {
		dropped = append(dropped, u.ID)
	}
	return dropped, brokenNote(broken)
}

// withReasons appends each id's recorded drop reason, e.g. "gpt-5 (deprecated 2026-11-20)".
func withReasons(p *providers.Provider, ids []string) []string {
	why, _ := DroppedReasons(p)
	out := slices.Clone(ids)
	for i, id := range out {
		if w := why[id]; w != "" {
			out[i] = id + " (" + w + ")"
		}
	}
	return out
}

// brokenNote names each id whose stream broke; none is dropped, the owner decides.
func brokenNote(broken []string) string {
	lines := make([]string, len(broken))
	for i, line := range broken {
		id, _, _ := strings.Cut(line, ":")
		lines[i] = fmt.Sprintf("stream check failed, not dropped: %s; to drop it: claude-code models discover --drop %s --apply", line, id)
	}
	return strings.Join(lines, "\n  ")
}

// probeCandidates are the ids discovery would add, plus p's saved additions when verify is set.
func probeCandidates(p *providers.Provider, live []providers.ModelInfo, verify bool) ([]string, error) {
	added, err := previewPickerModels(p, live)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range added {
		ids = append(ids, m.ID)
	}
	if verify {
		saved, err := PickerModels(p)
		if err != nil {
			return nil, err
		}
		for _, m := range saved {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

type unservedID struct{ ID, Reason string }

// unservedIDs sends ids one request on the route each speaks and returns those answered deprecated or gateway-only.
func unservedIDs(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, ids []string) (dropped []unservedID, broken []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	byID := make(map[string]providers.ModelInfo, len(live))
	for _, m := range live {
		byID[m.ID] = m
	}
	targets := make([]serving.CheckTarget, len(ids))
	for i, id := range ids {
		m := byID[id]
		resp := slices.Contains(m.APITypes, "openai/v1/responses")
		targets[i] = serving.CheckTarget{ID: id, Wire: id, Responses: resp, NoStream: !resp && m.Task != "" && m.Task != "llm/v1/chat"}
	}
	// the listing's 45 s deadline would cut a 30 s-per-request probe round short
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	results, err := probeEndpoints(ctx, p, targets, serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return nil, nil, err
	}
	dropped, broken = probeVerdicts(results)
	return dropped, broken, nil
}

func unservedAnswer(r serving.CheckResult) (reason string, ok bool) {
	id, reason, ok := serving.UnservedEndpoint(r.HTTP, r.Error)
	return reason, ok && id == r.ID
}

// probeVerdicts splits results into unserved ids and "id: reason" lines for streams that broke.
func probeVerdicts(results []serving.CheckResult) (dropped []unservedID, broken []string) {
	for _, r := range results {
		reason, unserved := unservedAnswer(r)
		switch {
		case unserved:
			dropped = append(dropped, unservedID{r.ID, reason})
		case r.Status == serving.StatusBroken:
			broken = append(broken, r.ID+": "+r.Error)
		}
	}
	return dropped, broken
}

// SpawnDiscover starts `models preflight --discover <profile>` detached (a goroutine would die at a launch's exec)
func SpawnDiscover(profile string, force bool) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	if !ClaimNamed("discover-"+url.PathEscape(profile), time.Now()) && !force {
		return false
	}
	return proc.SpawnDetached(self, []string{"models", "preflight", "--discover", profile}, "", null)
}

// DiscoverTargets is the profiles copilot and opencode point at that have models to discover (Databricks, GitHub).
func DiscoverTargets(reg *providers.Registry) []string {
	var out []string
	for _, agent := range []string{agentid.Copilot, agentid.OpenCode} {
		name := providers.AgentRecord(agent).EffectiveRoute().Profile
		if name == "" && agent == agentid.Copilot {
			name = githubProfile(reg)
		}
		p := reg.Providers[name]
		if p == nil || (p.Preset != providers.PresetDatabricks && p.Provider != "github") || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// githubProfile is the first profile on the github provider (copilot's default route), or "" when none exists.
func githubProfile(reg *providers.Registry) string {
	if reg == nil {
		return ""
	}
	if names := reg.ProfilesOf("github"); len(names) > 0 {
		return names[0]
	}
	return ""
}

// RecallOrSpawn is p's sealed models with no network if usable accepts them; an expired set is served and refreshed.
func RecallOrSpawn(ctx context.Context, p *providers.Provider, usable func([]providers.ModelInfo) bool,
	spawn func(profile string) bool) ([]providers.ModelInfo, bool) {
	snap, ok := recallSnapshot(ctx, p, usable, spawn)
	return snap.Models, ok
}

// recallSnapshot is RecallOrSpawn's snapshot, so a caller can read its DiscoveredAt too.
func recallSnapshot(ctx context.Context, p *providers.Provider, usable func([]providers.ModelInfo) bool,
	spawn func(profile string) bool) (serving.Snapshot, bool) {
	snap, ok := serving.RecallCtx(ctx, p)
	if !ok || !usable(snap.Models) {
		return serving.Snapshot{}, false
	}
	if time.Since(snap.DiscoveredAt) >= serving.CacheTTL() {
		spawn(p.Name)
	}
	return snap, true
}

// refreshRetry spaces detached refresh attempts, so an unreachable workspace doesn't spawn one per SessionStart.
const refreshRetry = 15 * time.Minute

// ClaimNamed takes the detached-refresh slot stamped name; false when another attempt started within refreshRetry.
func ClaimNamed(name string, now time.Time) bool { return ClaimEvery(name, refreshRetry, now) }

// ClaimEvery takes the slot stamped name; false when an attempt started within every (a fresh stamp costs a stat).
func ClaimEvery(name string, every time.Duration, now time.Time) bool {
	path := filepath.Join(paths.StateDir(), "preflight", name)
	if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < every {
		return false
	}
	claimed := true
	err := paths.WithFileLock(path, func() error {
		if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < every {
			claimed = false
			return nil
		}
		return paths.AtomicWriteNoSync(path, nil, 0o600)
	})
	_ = err // best effort: an unwritable stamp or lock must not stop the refresh
	return claimed
}

// pickerCatalog stores profile-scoped selectable history, not current availability.
type pickerCatalog struct {
	Schema int                   `json:"schema"`
	Scope  string                `json:"scope"`
	Models []providers.ModelInfo `json:"models"`
	// Dropped ids are never re-added by discovery until `models discover --undrop`.
	Dropped []string `json:"dropped,omitempty"`
	// DroppedWhy maps a dropped id to "deprecated <date>"; absent for a hand drop.
	DroppedWhy map[string]string `json:"dropped_why,omitempty"`
}

var _ = schemas.Register(schemas.Format{
	Name: "picker-additions", Owner: "internal/catalog", Kind: schemas.Store,
	Glob: vault.Path, Version: 1,
	Keep: "profile-scoped sealed picker-additions/v1 records inside the vault (additions, dropped ids); not live availability",
})

func pickerScope(p *providers.Provider) string {
	// Hash configuration without exposing auth commands or headers; models/pins are not scope.
	opts := p.Options
	opts.WarehouseID, opts.Prices = "", nil // cost settings are not routing: setting them must not orphan the drop list
	routes := p.Routes
	if host := providers.PresetHost(p.Host); p.Preset == providers.PresetDatabricks && host != "" && routes[providers.ProtocolGemini] == host+"/serving-endpoints/gemini" {
		routes = maps.Clone(routes)
		delete(routes, providers.ProtocolGemini) // preset-derived default: omitting it preserves existing sealed drop hashes
	}
	data, _ := json.Marshal([]any{p.Name, p.Provider, p.Login, p.Identity,
		p.Protocol, p.BaseURL, routes, p.Host, p.CLIProfile, p.SpellingRule(), opts,
		p.Auth, p.Headers})
	return textutil.SHA256Hex(data)
}

func pickerKey(p *providers.Provider) string { return "picker-additions/v1/" + pickerScope(p) }

func decodePicker(raw, scope string) (pickerCatalog, error) {
	var c pickerCatalog
	if json.Unmarshal([]byte(raw), &c) != nil || c.Schema != 1 || c.Scope != scope {
		return c, errors.New("invalid picker additions schema or scope; refusing to overwrite")
	}
	for _, m := range c.Models {
		if m.ID == "" {
			return c, errors.New("invalid picker additions entry; refusing to overwrite")
		}
	}
	return c, nil
}

// pickerRecord reads this profile's record; only a missing record counts as empty.
func pickerRecord(p *providers.Provider) (pickerCatalog, error) {
	raw, found, err := vault.ReadSecret(pickerKey(p))
	if err != nil {
		return pickerCatalog{}, errors.New("cannot read or decrypt saved picker additions")
	}
	if !found {
		return pickerCatalog{}, nil
	}
	return decodePicker(raw, pickerScope(p))
}

// PickerModels reads this profile's additions; only a missing record counts as empty.
func PickerModels(p *providers.Provider) ([]providers.ModelInfo, error) {
	c, err := pickerRecord(p)
	return c.Models, err
}

// PickerDropped reads the ids dropped from this profile's picker.
func PickerDropped(p *providers.Provider) ([]string, error) {
	c, err := pickerRecord(p)
	return c.Dropped, err
}

// pickerNewWithBaseline excludes configured aliases and the baseline menu using known wire equivalence.
func pickerNewWithBaseline(p *providers.Provider, saved, live []providers.ModelInfo, baseline []providers.OpencodeModel, dropped []string) []providers.ModelInfo {
	seen := pickerSeen(p, saved, baseline)
	drop := providers.MenuIDSet(dropped)
	var out []providers.ModelInfo
	for _, m := range live {
		if m.ID == "" || drop[providers.MenuID(m.ID)] || seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		if p.Preset == providers.PresetDatabricks && (!m.ReadyChat() || llama3Instruct(m.ID)) {
			continue
		}
		seen[p.WireModel(models.Strip1M(m.ID))] = true
		// Do not persist readiness or capabilities as a claim of current availability.
		out = append(out, providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: m.UseCase})
	}
	slices.SortFunc(out, func(a, b providers.ModelInfo) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// llama3Instruct is a llama 3.x instruct id, kept out of the catalog because it answers tool tasks wrong.
func llama3Instruct(id string) bool {
	id = strings.TrimPrefix(strings.ToLower(id), "databricks-")
	return strings.HasPrefix(id, "meta-llama-3-") && strings.HasSuffix(id, "-instruct")
}

// pickerSeen is the set of wire ids already offered: configured aliases, the legacy menu (Databricks), saved additions.
func pickerSeen(p *providers.Provider, saved []providers.ModelInfo, baseline []providers.OpencodeModel) map[string]bool {
	seen := make(map[string]bool)
	for _, id := range p.Models {
		seen[p.WireModel(models.Strip1M(id))] = true
	}
	if p.Preset == providers.PresetDatabricks {
		for _, m := range baseline {
			seen[p.WireModel(m.ID)] = true
		}
	}
	for _, m := range saved {
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return seen
}

// previewPickerModels computes additions without writing any discovery state.
func previewPickerModels(p *providers.Provider, live []providers.ModelInfo) ([]providers.ModelInfo, error) {
	c, err := pickerRecord(p)
	if err != nil {
		return nil, err
	}
	return pickerNewWithBaseline(p, c.Models, live, providers.OpencodeMenu(), c.Dropped), nil
}

// AppendPickerModels atomically appends sealed additions without losing concurrent updates.
func AppendPickerModels(p *providers.Provider, live []providers.ModelInfo) (added []providers.ModelInfo, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	baseline := providers.OpencodeMenu()
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		// UpdateSealed may retry after a concurrent key rotation.
		added = nil
		c, err := txPicker(tx, key, name, scope)
		if err != nil {
			return err
		}
		// Use the captured baseline to avoid a nested vault view inside this transaction.
		added = pickerNewWithBaseline(p, c.Models, live, baseline, c.Dropped)
		if len(added) == 0 {
			return nil
		}
		c.Models = providers.MergePickerModels(c.Models, added)
		data, err := json.Marshal(c)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return added, err
}

// txPicker opens the sealed record inside a transaction; a missing record is an empty catalog.
func txPicker(tx *vault.Tx, key *vault.Key, name, scope string) (pickerCatalog, error) {
	record, found, err := tx.Record(name)
	if err != nil {
		return pickerCatalog{}, errors.New("cannot read saved picker additions record")
	}
	if !found {
		return pickerCatalog{Schema: 1, Scope: scope}, nil
	}
	raw, err := key.Open(name, record)
	if err != nil {
		return pickerCatalog{}, errors.New("cannot decrypt saved picker additions; refusing to overwrite")
	}
	return decodePicker(raw, scope)
}

// applyDrop removes ids from c's additions and records them dropped; undrop clears the record instead.
func applyDrop(p *providers.Provider, c pickerCatalog, ids []string, undrop bool) (next pickerCatalog, removed []string) {
	want := providers.MenuIDSet(ids)
	next = c
	next.Schema, next.Scope = 1, pickerScope(p)
	if undrop {
		next.Dropped = slices.DeleteFunc(slices.Clone(c.Dropped), func(id string) bool { return want[providers.MenuID(id)] })
		next.DroppedWhy = maps.Clone(c.DroppedWhy)
		maps.DeleteFunc(next.DroppedWhy, func(id, _ string) bool { return want[providers.MenuID(id)] })
		return next, nil
	}
	next.Models = nil
	for _, m := range c.Models {
		if want[providers.MenuID(m.ID)] {
			removed = append(removed, m.ID)
			continue
		}
		next.Models = append(next.Models, m)
	}
	next.Dropped = slices.Clone(c.Dropped)
	for _, id := range ids {
		if !slices.Contains(next.Dropped, id) {
			next.Dropped = append(next.Dropped, id)
		}
	}
	return next, removed
}

// EditPickerDrops (un)drops ids on p's picker; write false previews; removed lists the additions a drop removes.
func EditPickerDrops(p *providers.Provider, ids []string, undrop, write bool) (removed []string, err error) {
	if !write {
		c, err := pickerRecord(p)
		if err != nil {
			return nil, err
		}
		_, removed = applyDrop(p, c, ids, undrop)
		return removed, nil
	}
	name, scope := pickerKey(p), pickerScope(p)
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		c, err := txPicker(tx, key, name, scope)
		if err != nil {
			return err
		}
		var next pickerCatalog
		next, removed = applyDrop(p, c, ids, undrop)
		unchanged := slices.Equal(c.Dropped, next.Dropped) && len(removed) == 0
		if unchanged || (!undrop && len(removed) == 0 && p.Preset != providers.PresetDatabricks) {
			return nil
		}
		data, err := json.Marshal(next)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return removed, err
}

// RecordDropped drops id from p's picker with the reason and date; added is false when it was already dropped.
func RecordDropped(p *providers.Provider, id, reason string, at time.Time) (added bool, err error) {
	name, scope := pickerKey(p), pickerScope(p)
	err = vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		added = false // UpdateSealed may retry
		c, err := txPicker(tx, key, name, scope)
		if err != nil || providers.MenuIDSet(c.Dropped)[providers.MenuID(id)] {
			return err
		}
		next, _ := applyDrop(p, c, []string{id}, false)
		next.DroppedWhy = maps.Clone(c.DroppedWhy)
		if next.DroppedWhy == nil {
			next.DroppedWhy = map[string]string{}
		}
		next.DroppedWhy[id] = reason + " " + at.UTC().Format(time.DateOnly)
		data, err := json.Marshal(next)
		if err != nil {
			return errors.New("cannot encode picker additions")
		}
		added = true
		return tx.PutSealed(key, name, string(data), "picker additions", time.Now().UTC())
	})
	return added, err
}

// DroppedReasons is p's recorded drop reasons by id ("deprecated 2026-11-20", "gateway-only 2026-11-21").
func DroppedReasons(p *providers.Provider) (map[string]string, error) {
	c, err := pickerRecord(p)
	return c.DroppedWhy, err
}

// droppedLive is the dropped ids of p that the live listing still serves, for discovery to report as skipped.
func droppedLive(p *providers.Provider, live []providers.ModelInfo) ([]string, error) {
	c, err := pickerRecord(p)
	if err != nil || len(c.Dropped) == 0 {
		return nil, err
	}
	drop := providers.MenuIDSet(c.Dropped)
	var out []string
	for _, m := range live {
		if drop[providers.MenuID(m.ID)] {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// OpencodeMenuFor appends profile additions with known protocol flags to the unchanged legacy menu.
func OpencodeMenuFor(p *providers.Provider) ([]providers.OpencodeModel, error) {
	out := slices.Clone(providers.OpencodeMenu())
	c, err := pickerRecord(p)
	if err != nil {
		return out, err
	}
	dropped := providers.MenuIDSet(c.Dropped)
	out = slices.DeleteFunc(out, func(m providers.OpencodeModel) bool { return dropped[providers.MenuID(m.ID)] })
	saved := c.Models
	seen := make(map[string]bool)
	for _, m := range out {
		seen[p.WireModel(m.ID)] = true
	}
	if p.Provider == "magnitude" { // a local server's menu is its pinned aliases; other rows keep their catalogs
		for _, alias := range models.AliasNames {
			if id := models.Strip1M(p.Models[alias]); id != "" && !seen[p.WireModel(id)] {
				out = append(out, providers.OpencodeModel{ID: id, Label: id, Responses: models.ResponsesOnly(id)})
				seen[p.WireModel(id)] = true
			}
		}
	}
	for _, m := range saved {
		if seen[p.WireModel(models.Strip1M(m.ID))] {
			continue
		}
		out = append(out, providers.OpencodeModel{ID: m.ID, Label: cmp.Or(m.Name, m.ID), Responses: models.ResponsesOnly(m.ID)})
		seen[p.WireModel(models.Strip1M(m.ID))] = true
	}
	return out, nil
}
