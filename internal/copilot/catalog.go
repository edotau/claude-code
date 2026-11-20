package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/copilot/sdk"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/github"
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

// SDKModel is the catalog entry the harness prints and validates pins against.
type SDKModel = sdk.Model

// sdkListModels is the seam tests stub to fake the catalog without a GitHub login.
var sdkListModels = sdk.ListModels

// launchCatalog is the SDK's raw catalog on token, the launch account's.
func launchCatalog(ctx context.Context, token string) ([]SDKModel, error) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	return sdkListModels(ctx, sdk.CatalogOptions(bin, token))
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

// RefreshProfile re-seals one profile's snapshot by its registry row: github or Databricks.
func RefreshProfile(ctx context.Context, name string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.Providers[name]
	switch {
	case catalog.IsGithubProfile(p):
		// that row's own account, not the pin's
		_, err = refreshCatalog(ctx, p.Login)
		return err
	case p != nil && p.Preset == providers.PresetDatabricks:
		_, err = relistCopilotEndpoints(ctx, reg, p)
		return err
	}
	return fmt.Errorf("%q is not a Databricks or github profile", name)
}

// DiscoverGithubModels lists a github row's live models from its own account's Copilot catalog (no pin switch).
func DiscoverGithubModels(ctx context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
	if note := catalog.ProfileCatalogSupport(p); note != "" {
		return nil, errors.New(note)
	}
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, errors.New("copilot CLI unavailable")
	}
	token := github.GHToken(p.Login)
	if token == "" {
		return nil, errors.New("requested GitHub account has no gh token")
	}
	list, err := sdkListModels(ctx, sdk.CatalogOptions(bin, token))
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
