package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// ListModels lists the GitHub Copilot catalog through the SDK on the launch account's token (its own login, never BYOK).
func ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	list, err := launchCatalog(ctx, CopilotToken())
	if err != nil {
		return nil, err
	}
	out := make([]providers.ModelInfo, len(list))
	for i, m := range list {
		out[i] = modelInfo(m)
	}
	return out, nil
}

// launchCatalog is the SDK's raw catalog on token, the launch account's.
func launchCatalog(ctx context.Context, token string) ([]SDKModel, error) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	return SDKListModels(ctx, CatalogOptions(bin, token))
}

// copilotCapabilities joins copilot's capability flags and premium-billing multiplier into one
// "vision · reasoning · x2" string; "" when the model carries none of them.
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

// catalogRecord is the sealed vault record holding the GitHub Copilot catalog snapshot.
const catalogRecord = "copilot-catalog"

// catalogSnapshot is the sealed catalog of one GitHub auth (account, credential source and token digest).
type catalogSnapshot struct {
	Schema       int
	Account      string
	Source       string
	TokenHash    string // sha256 of the token listed with; a changed token (e.g. env) must not reuse the catalog
	DiscoveredAt time.Time
	Models       []SDKModel
}

// readCatalogSnapshot is the sealed snapshot for auth and tok; ok is false on a miss, another account or any vault error.
func readCatalogSnapshot(auth CopilotAuth, tok string) (catalogSnapshot, bool) {
	raw, found, err := vault.ReadSecret(catalogRecord)
	if err != nil || !found {
		return catalogSnapshot{}, false
	}
	var s catalogSnapshot
	if json.Unmarshal([]byte(raw), &s) != nil || s.Schema != 1 || s.DiscoveredAt.IsZero() || len(s.Models) == 0 ||
		s.Account != auth.Account || s.Source != auth.Source || s.TokenHash != textutil.SHA256Hex([]byte(tok)) {
		return catalogSnapshot{}, false
	}
	return s, true
}

// catalogToken is auth's token for a listing; a gh-sourced account without one is an error, never another account's catalog.
func catalogToken(auth CopilotAuth) (string, error) {
	tok, _ := copilotToken(auth)
	if tok == "" && (auth.Source == srcPinned || auth.Source == srcActive) {
		return "", fmt.Errorf("copilot: no token for GitHub account %s; refusing to list another account's catalog", auth.Account)
	}
	return tok, nil
}

// saveCatalogSnapshot seals the snapshot under catalogRecord.
func saveCatalogSnapshot(s catalogSnapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return errors.New("cannot encode catalog snapshot")
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutRecord(catalogRecord, vault.Record{Sealed: key.Seal(catalogRecord, string(data)), Kid: key.Kid,
			Len: len(data), Label: "github/copilot", SetAt: s.DiscoveredAt})
	})
}

// Catalog is the copilot model catalog: the sealed snapshot of the current GitHub auth with no network (an expired one
// also spawns a detached refresh), else a live listing that is sealed.
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
	return RefreshCatalog(ctx)
}

// spawnCatalogRefresh starts a detached discovery for the GitHub profile, when the registry names one.
func spawnCatalogRefresh() {
	reg, err := providers.Load()
	if err != nil {
		return
	}
	if name := catalog.GithubProfile(reg); name != "" {
		catalog.SpawnDiscover(name, false)
	}
}

// RefreshCatalog lists the catalog live on the launch account's token and seals it, whatever the snapshot's age.
func RefreshCatalog(ctx context.Context) ([]SDKModel, error) {
	auth, ok := copilotAuthSource() // read once, so the listing and its seal name the same account
	tok := ""
	if ok {
		t, err := catalogToken(auth)
		if err != nil {
			return nil, err
		}
		tok = t
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
		_, err = RefreshCatalog(ctx)
		return err
	case p != nil && p.Preset == providers.PresetDatabricks:
		_, err = relistCopilotEndpoints(ctx, reg, p)
		return err
	}
	return fmt.Errorf("%q is not a Databricks or github profile", name)
}

// DiscoverySource is where p's models are discovered from and, when discovery cannot run, why not.
func DiscoverySource(p *providers.Provider) (source, skip string) {
	source, skip = providers.DiscoverySource(p)
	if isGithubProfile(p) {
		skip = profileCatalogSupport(p)
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

// DiscoverProfileModels lists p's live models: the Copilot catalog for github rows (no pin switch), else the gateway.
func DiscoverProfileModels(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
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
	var out []providers.ModelInfo
	for _, m := range list {
		if !m.Enabled {
			continue
		}
		out = append(out, modelInfo(m))
	}
	return out, nil
}

// Seams for tests: the forced relist and the detached refresh spawn.
var (
	copilotRelist       = relistCopilotEndpoints
	copilotSpawnRefresh = func(profile string) bool { return catalog.SpawnDiscover(profile, false) }
)

// copilotModelInfos is a launch's read of p's models: the sealed snapshot with no network when one is remembered,
// else the identity-checked read. An expired snapshot is served and refreshed by a detached process.
func copilotModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	if infos, ok := catalog.RecallOrSpawn(p, copilotUsable, copilotSpawnRefresh); ok {
		return infos, true, nil
	}
	return copilotCheckedModelInfos(ctx, reg, p)
}

// copilotUsable accepts a snapshot holding at least one chat row copilot can serve.
func copilotUsable(infos []providers.ModelInfo) bool {
	return len(copilotServingChatRows(infos, nil)) > 0
}

// copilotCheckedModelInfos reads p's models snapshot-first with the identity validated; a scope, identity or vault
// failure lists live as it always did.
func copilotCheckedModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	scope, old, found, err := copilotSnapshot(ctx, p)
	if err != nil {
		infos, err = providers.ListModels(ctx, p)
		return infos, false, err
	}
	if !found {
		infos, err = copilotRefresh(ctx, reg, p, scope, old)
		return infos, false, err
	}
	if time.Since(old.DiscoveredAt) < serving.CacheTTL() {
		copilotRemember(p, old)
		return old.Models, true, nil
	}
	listCtx, cancel := halfRemaining(ctx) // a failed refresh must leave the launch time to use the stale snapshot
	defer cancel()
	infos, err = copilotRefresh(listCtx, reg, p, scope, old)
	if err == nil || ctx.Err() != nil {
		return infos, false, err
	}
	fmt.Fprintf(os.Stderr, "copilot: %s live listing failed, using the %s-old snapshot: %s\n", p.Name,
		time.Since(old.DiscoveredAt).Round(time.Minute), serving.TruncateMsg([]byte(err.Error())))
	return old.Models, true, nil
}

// copilotRemember seals old again when p has no remembered subject or another one, so the next launch hits locally.
func copilotRemember(p *providers.Provider, old serving.Snapshot) {
	if subject, found, err := serving.RecallSubject(p); err == nil && found && subject == old.Scope.SubjectID {
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
	infos, err := copilotRefresh(ctx, reg, p, scope, old)
	if err != nil {
		return nil, err
	}
	return copilotServingChatRows(infos, providers.OpencodeMenu()), nil
}

// copilotRefresh lists live and seals, keeping old's Checks; a failed seal only costs the next launch a listing.
func copilotRefresh(ctx context.Context, reg *providers.Registry, p *providers.Provider, scope serving.Scope,
	old serving.Snapshot,
) ([]providers.ModelInfo, error) {
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return nil, err
	}
	snapshot := serving.Snapshot{Schema: 1, Scope: scope, DiscoveredAt: time.Now().UTC(), Models: infos,
		CheckedAt: old.CheckedAt, Checks: old.Checks}
	if err := serving.Save(p, snapshot); err != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s discovery snapshot not saved: %v\n", p.Name, err)
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
	old, found, err := serving.Read(scope, true) // true: an invalid payload reads as a miss, so a seal replaces it
	if err != nil {
		return serving.Scope{}, serving.Snapshot{}, false, err
	}
	return scope, old, found && len(copilotServingChatRows(old.Models, nil)) > 0, nil
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
	Tested   bool
	Snapshot bool // read from a sealed snapshot, so it can lag the workspace
}

// discoverCopilotEndpoints reads the resolved Databricks profile p's READY chat endpoints for a launch.
func discoverCopilotEndpoints(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]CopilotEndpoint, error) {
	return copilotEndpointRows(copilotModelInfos(ctx, reg, p))
}

// checkedCopilotEndpoints is discoverCopilotEndpoints with the identity validated on every read, for explicit commands.
func checkedCopilotEndpoints(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]CopilotEndpoint, error) {
	return copilotEndpointRows(copilotCheckedModelInfos(ctx, reg, p))
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

// copilotServingChatRows keeps READY llm/v1/chat endpoints in order, marking those on menu (with or without "databricks-").
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
		name = providers.SessionProfile()
	}
	p, ok := reg.Providers[name]
	if !ok || p == nil || p.Preset != providers.PresetDatabricks {
		return CopilotCatalog{Source: "GitHub Copilot catalog"}, nil
	}
	rows, err := checkedCopilotEndpoints(ctx, reg, p)
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true, Rows: rows}, nil
}

// modelInfo is m as a provider model row: its use case and capability string.
func modelInfo(m SDKModel) providers.ModelInfo {
	return providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: models.UseCase(m.ID, m.Name), Capabilities: copilotCapabilities(m)}
}

// DiscoverResult is one profile's `models discover` outcome.
type DiscoverResult struct {
	Provider     string                `json:"provider"`
	Profile      string                `json:"profile"`
	Source       string                `json:"source"`
	Discovered   int                   `json:"discovered"`
	Additions    []providers.ModelInfo `json:"additions"`
	Destinations []string              `json:"destinations"`
	Status       string                `json:"status"`
	Note         string                `json:"note,omitempty"`
}

// DiscoverPickerProfile live-discovers p's models and previews (or, with apply, appends) the saved picker additions.
func DiscoverPickerProfile(p *providers.Provider, apply bool) DiscoverResult {
	r := DiscoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := DiscoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, err := DiscoverProfileModels(ctx, p)
	if err != nil {
		r.Status = "error"
		r.Note = err.Error() + " (no fallback saved)"
		return r
	}
	r.Discovered = len(live)
	r.Destinations = discoverDestinations(p)
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

// DiscoverPickerProfiles runs DiscoverPickerProfile over names; ok is false when one errored or none completed.
func DiscoverPickerProfiles(reg *providers.Registry, names []string, apply bool) (results []DiscoverResult, ok bool) {
	results = make([]DiscoverResult, 0, len(names))
	ok = true
	completed := 0
	for _, name := range names {
		p, _ := reg.Get(name)
		r := DiscoverPickerProfile(p, apply)
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
