package copilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
)

// Seams for tests: the forced relist and the detached refresh spawn.
var (
	copilotRelist       = relistCopilotEndpoints
	copilotSpawnRefresh = func(profile string) bool { return preflight.SpawnDiscover(profile, false) }
)

// copilotModelInfos is a launch's read of p's models: the sealed snapshot with no network when one is remembered,
// else the identity-checked read. An expired snapshot is served and refreshed by a detached process.
func copilotModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	if infos, ok := copilotCachedModels(p); ok {
		return infos, true, nil
	}
	return copilotCheckedModelInfos(ctx, reg, p)
}

// copilotCachedModels reads p's snapshot under its remembered subject, locally; ok is false on any miss or vault error.
func copilotCachedModels(p *providers.Provider) (infos []providers.ModelInfo, ok bool) {
	old, found := serving.Recall(p)
	if !found || len(copilotServingChatRows(old.Models, nil)) == 0 {
		return nil, false
	}
	expired := time.Since(old.DiscoveredAt) >= serving.CacheTTL()
	if expired {
		copilotSpawnRefresh(p.Name)
	}
	return old.Models, true
}

// RefreshCopilotModels is the detached refresh: validate profile's identity, relist and seal; the launch never waits.
func RefreshCopilotModels(ctx context.Context, profile string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks {
		return fmt.Errorf("copilot: %q is not a Databricks profile", profile)
	}
	_, err = relistCopilotEndpoints(ctx, reg, p)
	return err
}

// copilotCheckedModelInfos reads p's models snapshot-first with the identity validated; a scope, identity or vault
// failure lists live as it always did.
func copilotCheckedModelInfos(ctx context.Context, reg *providers.Registry, p *providers.Provider,
) (infos []providers.ModelInfo, snapshot bool, err error) {
	scope, old, found, err := copilotSnapshot(ctx, p)
	if err != nil {
		infos, err = providers.ListModels(ctx, reg, p)
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

// copilotSelect builds the registry and selects c; a model miss on a snapshot list relists and retries once.
func copilotSelect(ctx context.Context, reg *providers.Registry, p, routed *providers.Provider, eps []CopilotEndpoint,
	base string, c CopilotChoice,
) (CopilotRegistry, string, error) {
	r := copilotBuildRegistry(p.Name, routed, eps, base)
	ref, err := r.Select(c)
	fromSnapshot := slices.ContainsFunc(eps, func(ep CopilotEndpoint) bool { return ep.Snapshot })
	if (!errors.Is(err, ErrUnknownModel) && !errors.Is(err, ErrSavedModelGone)) || !fromSnapshot {
		return r, ref, err
	}
	fresh, listErr := copilotRelist(ctx, reg, p)
	if listErr != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s relist failed: %s\n", p.Name, serving.TruncateMsg([]byte(listErr.Error())))
		return r, "", err
	}
	r = copilotBuildRegistry(p.Name, routed, fresh, base)
	ref, err = r.Select(c)
	return r, ref, err
}

// copilotRefresh lists live and seals, keeping old's Checks; a failed seal only costs the next launch a listing.
func copilotRefresh(ctx context.Context, reg *providers.Registry, p *providers.Provider, scope serving.Scope,
	old serving.Snapshot,
) ([]providers.ModelInfo, error) {
	infos, err := providers.ListModels(ctx, reg, p)
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
