package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// ListModels lists the GitHub Copilot catalog through the SDK on the launch account's token (never BYOK).
func ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	_, tok, _, err := catalogAuth("")
	if err != nil {
		return nil, err
	}
	list, err := launchCatalog(ctx, tok)
	if err != nil {
		return nil, err
	}
	return enabledModelInfos(list), nil
}

// enabledModelInfos is the catalog's policy-enabled models as provider rows.
func enabledModelInfos(list []SDKModel) []providers.ModelInfo {
	out := make([]providers.ModelInfo, 0, len(list))
	for _, m := range list {
		if m.Enabled {
			out = append(out, providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: models.UseCase(m.ID, m.Name),
				Capabilities: copilotCapabilities(m)})
		}
	}
	return out
}

// launchCatalog is the SDK's raw catalog on token, the launch account's.
func launchCatalog(ctx context.Context, token string) ([]SDKModel, error) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	return SDKListModels(ctx, CatalogOptions(bin, token))
}

// copilotCapabilities is m's flags and billing multiplier as "vision · reasoning · x2"; "" when it has none.
func copilotCapabilities(m SDKModel) string {
	var parts []string
	if m.Vision {
		parts = append(parts, "vision")
	}
	if m.ReasoningEffort {
		parts = append(parts, "reasoning")
	}
	if m.Multiplier != nil {
		parts = append(parts, fmt.Sprintf("x%g", *m.Multiplier))
	}
	return strings.Join(parts, " · ")
}

// catalogRecord is the sealed vault record holding the latest GitHub Copilot catalog snapshot, any account.
const catalogRecord = "copilot-catalog"

// accountRecord is account's own snapshot record, so two GitHub login rows never evict each other's listing.
func accountRecord(account string) string { return catalogRecord + "/" + account }

// catalogSnapshot is the sealed catalog of one GitHub auth (account, credential source and token digest).
type catalogSnapshot struct {
	Schema  int
	Account string
	Source  string
	// sha256 of the token listed with; a changed token (e.g. env) must not reuse the catalog
	TokenHash    string
	DiscoveredAt time.Time
	Models       []SDKModel
}

// readCatalogSnapshot is the sealed snapshot for auth and tok; ok false on a miss, another account or a vault error.
func readCatalogSnapshot(auth copilotAuth, tok string) (catalogSnapshot, bool) {
	raw, found, err := vault.ReadSecret(accountRecord(auth.Account))
	if err != nil || !found {
		return catalogSnapshot{}, false
	}
	var s catalogSnapshot
	if json.Unmarshal([]byte(raw), &s) != nil || s.Schema != 1 || s.DiscoveredAt.IsZero() ||
		s.Account != auth.Account || s.Source != auth.Source || s.TokenHash != textutil.SHA256Hex([]byte(tok)) {
		return catalogSnapshot{}, false
	}
	return s, true
}

// catalogToken is auth's listing token; a gh-sourced account without one errors, never another account's catalog.
func catalogToken(auth copilotAuth) (string, error) {
	tok, _ := copilotToken(auth)
	if tok == "" && (auth.Source == srcPinned || auth.Source == srcActive) {
		return "", fmt.Errorf("copilot: no token for GitHub account %s; refusing to list another account's catalog", auth.Account)
	}
	return tok, nil
}

// catalogAuth is the account a listing runs on: login's gh account, else the pin's; ok false = the CLI's own login.
func catalogAuth(login string) (auth copilotAuth, tok string, ok bool, err error) {
	if auth, ok, err = launchAuth(login); err != nil || !ok {
		return auth, "", false, err
	}
	tok, err = catalogToken(auth)
	return auth, tok, true, err
}

// saveCatalogSnapshot seals the snapshot under its account's record and catalogRecord.
func saveCatalogSnapshot(s catalogSnapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("cannot encode catalog snapshot")
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		if err := tx.PutSealed(key, accountRecord(s.Account), string(data), "github/copilot", s.DiscoveredAt); err != nil {
			return err
		}
		return tx.PutSealed(key, catalogRecord, string(data), "github/copilot", s.DiscoveredAt)
	})
}

// Catalog is the current GitHub auth's sealed snapshot (expired: also a detached refresh), else a sealed live listing.
func Catalog(ctx context.Context) ([]SDKModel, error) {
	if auth, ok := copilotAuthSource(); ok {
		if tok, err := catalogToken(auth); err == nil {
			if s, hit := readCatalogSnapshot(auth, tok); hit {
				if time.Since(s.DiscoveredAt) >= serving.CacheTTL() {
					spawnCatalogRefresh()
				}
				return s.Models, nil
			}
		}
	}
	return refreshCatalog(ctx, "")
}

var (
	snapshotMu  sync.Mutex
	snapshotMap = map[string]catalogIDs{} // per row login, read once per process on its first family name
)

type catalogIDs struct {
	ids []string
	at  time.Time
}

func init() {
	router.GithubCatalogIDs = snapshotIDs
	router.GithubCatalogTime = func(p *providers.Provider) time.Time { return snapshotFor(p).at }
	router.ResetGithubCatalog = func() {
		snapshotMu.Lock()
		defer snapshotMu.Unlock()
		clear(snapshotMap)
	}
}

// snapshotIDs is readSnapshotIDs for p's login, memoized per login.
func snapshotIDs(p *providers.Provider) []string { return snapshotFor(p).ids }

func snapshotFor(p *providers.Provider) catalogIDs {
	login := ""
	if p != nil {
		login = p.Login
	}
	snapshotMu.Lock()
	defer snapshotMu.Unlock()
	ids, ok := snapshotMap[login]
	if !ok {
		ids = readSnapshotIDs(login)
		snapshotMap[login] = ids
	}
	return ids
}

// readSnapshotIDs preserves the sealed snapshot timestamp even when no model is enabled.
func readSnapshotIDs(login string) catalogIDs {
	auth, tok, ok, err := catalogAuth(login)
	if !ok || err != nil {
		return catalogIDs{}
	}
	s, hit := readCatalogSnapshot(auth, tok)
	if !hit {
		return catalogIDs{}
	}
	var ids []string
	for _, m := range s.Models {
		if m.Enabled {
			ids = append(ids, m.ID)
		}
	}
	return catalogIDs{ids: ids, at: s.DiscoveredAt}
}

// spawnCatalogRefresh starts a detached discovery for copilot's own GitHub profile, when the registry names one.
func spawnCatalogRefresh() {
	reg, err := providers.Load()
	if err != nil {
		return
	}
	if p, ok := copilotGithubRow(reg); ok && p != nil {
		catalog.SpawnDiscover(p.Name, false)
	}
}

// refreshCatalog lists live on login's account ("" = the launch account's) and seals it, whatever the snapshot's age.
func refreshCatalog(ctx context.Context, login string) ([]SDKModel, error) {
	// read once, so the listing and its seal name the same account
	auth, tok, ok, err := catalogAuth(login)
	if err != nil {
		return nil, err
	}
	list, err := launchCatalog(ctx, tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return list, nil
	}
	snap := catalogSnapshot{Schema: 1, Account: auth.Account, Source: auth.Source, TokenHash: textutil.SHA256Hex([]byte(tok)),
		DiscoveredAt: time.Now().UTC(), Models: list}
	if err := saveCatalogSnapshot(snap); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: catalog snapshot not saved: %v\n", err)
	}
	return list, nil
}

// isGithubProfile is true for a registry row whose models come from the Copilot catalog, not a gateway listing.
func isGithubProfile(p *providers.Provider) bool { return p != nil && p.Provider == "github" }

// RefreshProfile re-seals one profile's snapshot by its registry row: github or Databricks.
func RefreshProfile(ctx context.Context, name string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.Providers[name]
	switch {
	case isGithubProfile(p):
		// that row's own account, not the pin's
		_, err = refreshCatalog(ctx, p.Login)
		return err
	case p != nil && p.Preset == providers.PresetDatabricks:
		_, err = relistCopilotEndpoints(ctx, reg, p)
		return err
	}
	return fmt.Errorf("%q is not a Databricks or github profile", name)
}

// discoverySource is where p's models are discovered from and, when discovery cannot run, why not.
func discoverySource(p *providers.Provider) (source, skip string) {
	source, skip = providers.DiscoverySource(p)
	if isGithubProfile(p) {
		skip = profileCatalogSupport(p)
	}
	if source == "" && skip != "" {
		skip += "; choose its models in the vendor CLI itself"
	}
	return source, skip
}

// profileCatalogSupport requires an explicit account for discovery without launch-account fallback.
func profileCatalogSupport(p *providers.Provider) string {
	if strings.TrimSpace(p.Login) == "" {
		return "Copilot discovery requires an explicit profile login; implicit launch-account fallback is disabled"
	}
	return ""
}

// discoverProfileModels lists p's live models: the Copilot catalog for github rows (no pin switch), else the gateway.
func discoverProfileModels(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
	if !isGithubProfile(p) {
		return providers.DiscoverModels(ctx, p)
	}
	if note := profileCatalogSupport(p); note != "" {
		return nil, errors.New(note)
	}
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, errors.New("copilot CLI unavailable")
	}
	token := GHToken(p.Login)
	if token == "" {
		return nil, errors.New("requested GitHub account has no gh token")
	}
	list, err := SDKListModels(ctx, CatalogOptions(bin, token))
	if err != nil {
		return nil, errors.New("requested account Copilot catalog unavailable")
	}
	return enabledModelInfos(list), nil
}

// copilotModelInfos is p's models for a launch: a remembered snapshot (expired: refreshed detached), else checked.
func copilotModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	spawn := func(profile string) bool { return catalog.SpawnDiscover(profile, false) }
	if infos, ok := catalog.RecallOrSpawn(ctx, p, copilotUsable, spawn); ok {
		return infos, true, nil
	}
	return copilotCheckedModelInfos(ctx, reg, p)
}

// copilotUsable accepts a snapshot holding at least one chat row copilot can serve.
func copilotUsable(infos []providers.ModelInfo) bool {
	for _, info := range infos {
		if info.ReadyChat() {
			return true
		}
	}
	return false
}

// copilotCheckedModelInfos reads p's models snapshot-first, identity checked; a scope/identity/vault error lists live.
func copilotCheckedModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	scope, old, found, err := copilotSnapshot(ctx, p)
	if err != nil {
		infos, err = providers.ListModels(ctx, p)
		return infos, false, err
	}
	if !found {
		infos, err = copilotRefresh(ctx, p, scope, old)
		return infos, false, err
	}
	if time.Since(old.DiscoveredAt) < serving.CacheTTL() {
		copilotRemember(ctx, p, old)
		return old.Models, true, nil
	}
	// a failed refresh must leave the launch time to use the stale snapshot
	listCtx, cancel := halfRemaining(ctx)
	defer cancel()
	infos, err = copilotRefresh(listCtx, p, scope, old)
	if err == nil || ctx.Err() != nil {
		return infos, false, err
	}
	fmt.Fprintf(os.Stderr, "copilot: %s live listing failed, using the %s-old snapshot: %s\n", p.Name,
		time.Since(old.DiscoveredAt).Round(time.Minute), serving.TruncateMsg([]byte(err.Error())))
	return old.Models, true, nil
}

// copilotRemember seals old again when p has no remembered subject or another one, so the next launch hits locally.
func copilotRemember(ctx context.Context, p *providers.Provider, old serving.Snapshot) {
	if subject, found, err := serving.RecallSubjectCtx(ctx, p); err == nil && found && subject == old.Scope.SubjectID {
		return
	}
	if err := serving.Save(p, old); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s discovery snapshot not saved: %v\n", p.Name, err)
	}
}

// relistCopilotEndpoints revalidates the subject, then lists live and seals, whatever the snapshot's age.
func relistCopilotEndpoints(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]CopilotEndpoint, error) {
	scope, old, _, err := copilotSnapshot(ctx, p)
	if err != nil {
		return nil, err
	}
	infos, err := copilotRefresh(ctx, p, scope, old)
	if err != nil {
		return nil, err
	}
	return copilotServingChatRows(infos, providers.OpencodeMenu()), nil
}

// copilotRefresh lists live and seals, keeping old's Checks; a failed seal only costs the next launch a listing.
func copilotRefresh(ctx context.Context, p *providers.Provider, scope serving.Scope,
	old serving.Snapshot,
) ([]providers.ModelInfo, error) {
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return nil, err
	}
	snapshot := serving.Snapshot{Schema: 1, Scope: scope, DiscoveredAt: time.Now().UTC(), Models: infos,
		CheckedAt: old.CheckedAt, Checks: old.Checks}
	if _, err := catalog.Seal(p, snapshot, false); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s discovery not saved: %v\n", p.Name, err)
	}
	return infos, nil
}

// copilotSnapshot validates the subject, then reads its snapshot; found is false when it holds no chat endpoint.
func copilotSnapshot(ctx context.Context, p *providers.Provider) (serving.Scope, serving.Snapshot, bool, error) {
	idCtx, cancel := halfRemaining(ctx)
	defer cancel()
	subject, err := serving.ValidateIdentity(idCtx, p)
	if err != nil {
		return serving.Scope{}, serving.Snapshot{}, false, err
	}
	scope, err := serving.NewScope(p, subject)
	if err != nil {
		return serving.Scope{}, serving.Snapshot{}, false, err
	}
	// true: an invalid payload reads as a miss, so a seal replaces it
	old, found, err := serving.Read(scope, true)
	if err != nil {
		return serving.Scope{}, serving.Snapshot{}, false, err
	}
	return scope, old, found && copilotUsable(old.Models), nil
}

// halfRemaining caps one step at half of ctx's remaining time, so it cannot starve the step after it.
func halfRemaining(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Until(deadline)/2)
}

// CopilotEndpoint is one serving endpoint copilot can reach; Tested marks endpoints on the curated menu.
type CopilotEndpoint struct {
	providers.ModelInfo
	Tested bool
	// read from a sealed snapshot, so it can lag the workspace
	Snapshot bool
}

// discoverCopilotEndpoints reads the resolved Databricks profile p's READY chat endpoints for a launch.
func discoverCopilotEndpoints(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]CopilotEndpoint, error) {
	return copilotEndpointRows(copilotModelInfos(ctx, reg, p))
}

// copilotEndpointRows keeps the READY chat endpoints of a model read, marking the curated-menu ones.
func copilotEndpointRows(infos []providers.ModelInfo, snapshot bool, err error) ([]CopilotEndpoint, error) {
	if err != nil {
		return nil, err
	}
	rows := copilotServingChatRows(infos, providers.OpencodeMenu())
	for i := range rows {
		rows[i].Snapshot = snapshot
	}
	return rows, nil
}

// copilotServingChatRows keeps READY llm/v1/chat endpoints in order, marking menu ones (with or without "databricks-").
func copilotServingChatRows(infos []providers.ModelInfo, menu []providers.OpencodeModel) []CopilotEndpoint {
	onMenu := make(map[string]bool, len(menu))
	for _, m := range menu {
		onMenu[m.ID] = true
	}
	var out []CopilotEndpoint
	for _, info := range infos {
		if !info.ReadyChat() {
			continue
		}
		tested := onMenu[info.ID] || onMenu[strings.TrimPrefix(info.ID, models.DatabricksPrefix)]
		out = append(out, CopilotEndpoint{ModelInfo: info, Tested: tested})
	}
	return out
}

// CopilotCatalog is where `copilot models` lists from: a Databricks row's endpoints, or GitHub's own catalog.
type CopilotCatalog struct {
	Source     string
	Profile    string
	Databricks bool
	Rows       []CopilotEndpoint
}

// CopilotModelSource resolves profile (default: the session profile) and lists its chat endpoints if Databricks.
func CopilotModelSource(ctx context.Context, reg *providers.Registry, profile string) (CopilotCatalog, error) {
	name := profile
	if name == "" {
		name = providers.SessionProfileContext(ctx)
	}
	p, ok := reg.Providers[name]
	if !ok || p == nil || p.Preset != providers.PresetDatabricks {
		return CopilotCatalog{Source: "GitHub Copilot catalog"}, nil
	}
	// identity validated on every read
	rows, err := copilotEndpointRows(copilotCheckedModelInfos(ctx, reg, p))
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true, Rows: rows}, nil
}

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

// discoverLive is the live catalog call; tests swap it for a canned listing.
var discoverLive = discoverProfileModels

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
func discoverUsable(ctx context.Context, p *providers.Provider) (live []providers.ModelInfo, note string, err error) {
	if note = missingKeyNote(ctx, p); note != "" {
		return nil, note, nil
	}
	if live, err = discoverLive(ctx, p); err == nil && len(live) == 0 {
		note = "the catalog listed no models for this account; " + emptyCatalogStep(p)
	}
	return live, note, err
}

// emptyCatalogStep is what to check when p's live catalog lists nothing.
func emptyCatalogStep(p *providers.Provider) string {
	if isGithubProfile(p) {
		return "check `gh auth status` and that " + cmp.Or(p.Login, "the account") + " holds a Copilot seat"
	}
	return "check the profile's credential with `claude-code doctor`"
}

// discoverPickerProfile live-discovers p's models and previews (or, with apply, appends) the saved picker additions.
func discoverPickerProfile(p *providers.Provider, apply, verify bool) DiscoverResult {
	r := DiscoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := discoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, note, err := discoverUsable(ctx, p)
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
	if r.Dropped, err = providers.DroppedLive(p, live); err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	r.Dropped = withReasons(p, r.Dropped)
	r.Deprecated, r.ProbeNote = databricksListing(ctx, p, live, apply, verify)
	var added []providers.ModelInfo
	if apply {
		added, err = providers.AppendPickerModels(p, live)
	} else {
		added, err = providers.PreviewPickerModels(p, live)
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

// DiscoverPickerProfiles runs discoverPickerProfile over names; ok is false when one errored or none completed.
func DiscoverPickerProfiles(reg *providers.Registry, names []string, apply, verify bool) (results []DiscoverResult, ok bool) {
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
		r := discoverPickerProfile(p, apply, verify)
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
			removed, err = providers.EditPickerDrops(p, ids, undrop, write)
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

// databricksListing keeps an applied listing's api_types and limits, then drops the ids the probe finds deprecated.
func databricksListing(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, apply, verify bool) (dropped []string, note string) {
	if p.Preset != providers.PresetDatabricks {
		return nil, ""
	}
	if !apply {
		return nil, "no deprecation probe sent in a preview; --apply probes new ids (--verify also the saved ones)"
	}
	if err := router.WarmServed(p.Name, live, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s served listing not saved: %v\n", p.Name, err)
	}
	ids, err := probeCandidates(p, live, verify)
	if err != nil {
		return nil, "deprecation probe skipped: " + err.Error()
	}
	dropped, broken, err := deprecatedIDs(ctx, p, live, ids)
	for i := 0; err == nil && i < len(dropped); i++ {
		_, err = providers.RecordDeprecated(p, dropped[i], time.Now())
	}
	if err != nil {
		return nil, "deprecation probe changed nothing: " + err.Error()
	}
	return dropped, brokenNote(broken)
}

// withReasons appends each id's recorded drop reason, e.g. "gpt-5 (deprecated 2026-11-20)".
func withReasons(p *providers.Provider, ids []string) []string {
	why, _ := providers.DroppedReasons(p)
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
	added, err := providers.PreviewPickerModels(p, live)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range added {
		ids = append(ids, m.ID)
	}
	if verify {
		saved, err := providers.PickerModels(p)
		if err != nil {
			return nil, err
		}
		for _, m := range saved {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// deprecatedIDs sends ids one request on the route each speaks and returns those answered 400 "This endpoint <id> is deprecated".
func deprecatedIDs(ctx context.Context, p *providers.Provider, live []providers.ModelInfo, ids []string) (dropped, broken []string, err error) {
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

func deprecatedAnswer(r serving.CheckResult) bool {
	id, ok := serving.DeprecatedEndpoint(r.HTTP, r.Error)
	return ok && id == r.ID
}

// probeVerdicts splits results into deprecated ids and "id: reason" lines for streams that broke.
func probeVerdicts(results []serving.CheckResult) (dropped, broken []string) {
	for _, r := range results {
		switch {
		case deprecatedAnswer(r):
			dropped = append(dropped, r.ID)
		case r.Status == serving.StatusBroken:
			broken = append(broken, r.ID+": "+r.Error)
		}
	}
	return dropped, broken
}
