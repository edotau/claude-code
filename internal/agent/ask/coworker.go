package ask

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
)

// ApplyCoworker is ctx with the confinement as the minimal $COWORKER_OVERLAY, databricks:<p> also as CLAUDE_PROFILE.
func ApplyCoworker(ctx context.Context, value, profile, agentName, model, fallback string) (context.Context, error) {
	ip, iprof, err := router.CoworkerContext(ctx)
	if err != nil {
		return ctx, err
	}
	provider, p, err := router.ParseCoworker(value)
	switch {
	case err != nil:
		return ctx, err
	case provider == "": // an inherited non-Databricks confinement is re-checked for agentName; a workspace was applied
		if ip == "" || ip == router.RouteDatabricks {
			return ctx, nil
		}
		return applyProvider(ctx, ip, iprof, agentName)
	}
	if err := checkLeave(ip, iprof, provider, p); err != nil {
		return ctx, err
	}
	if provider != router.RouteDatabricks {
		return applyProvider(ctx, provider, cmp.Or(p, iprof), agentName)
	}
	return applyArm(ctx, p, profile, agentName, model, fallback)
}

// withValues is ctx whose run values are set laid over its own: what the run exports, with nothing set process-wide.
func withValues(ctx context.Context, set proc.Values) context.Context {
	_, values := agent.RunValues(ctx, nil)
	return proc.WithValues(ctx, values.Merge(set))
}

// confinement is $COWORKER_OVERLAY as provider[:p] alone: the router and nested dispatches read it there.
func confinement(provider, p string) proc.Values {
	return proc.Values{agentconfig.OverlayVar: agentconfig.InlineOverlay(provider, p)}
}

// checkLeave refuses a provider or pinned profile other than the inherited confinement's; applyArm compares workspaces.
func checkLeave(ip, iprof, provider, p string) error {
	switch {
	case ip == router.RouteDatabricks && provider != router.RouteDatabricks,
		ip != "" && ip != router.RouteDatabricks && (provider != ip || (iprof != "" && p != "" && p != iprof)):
		return errLeave(ip, iprof)
	}
	return nil
}

// applyProvider checks profile p and agentName against a non-Databricks provider, then sets the confinement on ctx.
func applyProvider(ctx context.Context, provider, p, agentName string) (context.Context, error) {
	reg, err := providers.Load()
	if err != nil {
		return ctx, fmt.Errorf("overlay provider %s: %w", provider, err)
	}
	if row := reg.Providers[p]; p != "" && (row == nil || row.Provider != provider) {
		return ctx, fmt.Errorf("overlay profile: not a profile of %s", provider) // never echo the document's value
	}
	if err := canUse(reg, agentName, provider); err != nil {
		return ctx, err
	}
	return withValues(ctx, confinement(provider, p)), nil
}

// canUse refuses a table agent with no account on provider; another agent name (a batch's "") passes.
func canUse(reg *providers.Registry, agentName, provider string) error {
	if slices.Contains(router.TableAgents, agentName) && !router.CanUse(reg, agentName, provider) {
		return fmt.Errorf("overlay provider %s: %s has no %s account", provider, agentName, provider)
	}
	return nil
}

// confinedAgent is canUse for agentName against the non-Databricks confinement in ctx; nil outside one.
func confinedAgent(ctx context.Context, agentName string) error {
	cp, _ := router.ConfinedContext(ctx)
	if cp == "" || !slices.Contains(router.TableAgents, agentName) {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("overlay provider %s: %w", cp, err)
	}
	return canUse(reg, agentName, cp)
}

// confineLine fills a batch line's untyped lead for its own agent, as checkArm and resolveLead do for a single ask.
func confineLine(ctx context.Context, agentName string, req agent.RunOptions) (agent.RunOptions, error) {
	err := cmp.Or(checkNative(ctx, req.Native), confinedAgent(ctx, agentName), CheckArm(ctx, agentName, req.Provider, "", req.Model, false))
	if err != nil {
		return req, err // the line's own profile and model, checked like the typed flags
	}
	profile, provider := router.ConfineLeadContext(ctx, agentName, req.Provider, "", req.Model)
	if req.Lead != "" || (profile == req.Provider && provider == "") {
		return req, nil
	}
	q := agent.RouteReq{Agent: agentName, Model: req.Model, Profile: profile, Provider: provider, Fallback: req.Fallback}
	lead, prov, err := routedLead(q, agent.VaultAgentIsClaude(agentName))
	if err != nil {
		return req, err
	}
	req.Lead, req.Provider = lead, prov
	return req, nil
}

// routedLead is q's lead and provider (agent.LeadFor); readsProvider pins the lead on the provider instead.
func routedLead(q agent.RouteReq, readsProvider bool) (lead, provider string, err error) {
	if profile, qualified := strings.CutPrefix(q.Profile, router.RouteDatabricks+":"); qualified {
		q.Profile = router.RouteDatabricks + "·" + profile
	}
	lead, pin, err := agent.LeadFor(q)
	switch {
	case err != nil && !pin: // a bad pin fails at Prepare like any other backend error
		return "", q.Profile, err
	case lead == "" || readsProvider: // claude and --live holders read Provider, never Lead
		return "", cmp.Or(lead, q.Profile), nil
	}
	return lead, "", nil
}

// armValues is the run values of workspace p: its confinement and session profile.
func armValues(p string) proc.Values {
	return confinement(router.RouteDatabricks, p).With(providers.EnvSessionProfile, p)
}

// applyArm resolves databricks profile <p> (bare: from profile, else CLAUDE_PROFILE), checks it, sets it on ctx.
func applyArm(ctx context.Context, p, profile, agentName, model, fallback string) (context.Context, error) {
	p, explicit, err := armWorkspace(ctx, p, profile, agentName, model, fallback)
	if err != nil {
		return ctx, err
	}
	if err := checkArmRow(ctx, p, explicit, fallback); err != nil {
		return ctx, err
	}
	return withValues(ctx, armValues(p)), nil
}

// armWorkspace is the workspace databricks:<p> names (bare: bareArm, auto: autoWorkspace); explicit: neither auto.
func armWorkspace(ctx context.Context, p, profile, agentName, model, fallback string) (string, bool, error) {
	if p == "" {
		if p = bareArm(ctx, profile); p == "" {
			return "", false, errors.New("overlay provider databricks needs a profile: a workspace or auto")
		}
	}
	if p != router.AutoArm {
		return p, true, nil
	}
	p, err := autoWorkspace(ctx, agentName, model, fallback)
	return p, false, err
}

// checkArmRow refuses leaving the inherited workspace, a non-serving row, and an explicit one failing its preflight.
func checkArmRow(ctx context.Context, p string, explicit bool, fallback string) error {
	inherited := router.ArmProfileContext(ctx)
	if inherited != "" && p != inherited {
		return errLeave(router.RouteDatabricks, inherited)
	}
	if err := providers.ServingRow(p); err != nil {
		return err
	}
	if explicit && inherited == "" && fallback == router.FallbackNone {
		return workspaceRefusal(p)
	}
	return nil
}

// workspaceRefusal is the PreflightError of an explicit workspace failing its local check; nil when it passes.
func workspaceRefusal(name string) error {
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("overlay databricks:%s: %w", name, err)
	}
	if fs := agent.CheckWorkspace(reg.Providers[name], router.LoadState(), time.Now()); len(fs) > 0 {
		return &agent.PreflightError{Finding: fs[0]}
	}
	return nil
}

// autoWorkspace resolves databricks:auto: the inherited confinement's workspace inside one, else pickArm's.
func autoWorkspace(ctx context.Context, agentName, model, fallback string) (string, error) {
	if arm := router.ArmProfileContext(ctx); arm != "" {
		return arm, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("overlay databricks:auto: %w", err)
	}
	q := armQuery{agentName: agentName, model: model, strict: fallback == router.FallbackNone && typedModel(model)}
	return reportPick(pickWith(reg, q, time.Now()))
}

// typedModel reports a concrete id or family word: not empty, an alias, auto or a row-qualified spelling.
func typedModel(model string) bool {
	_, auto := agent.SplitAuto(model)
	return model != "" && !auto && !strings.Contains(model, ":") && !slices.Contains(router.Aliases, models.Strip1M(model))
}

// unservedError is the refusal of a typed model no signed-in workspace serves; it exits 1 like a preflight.
type unservedError struct{ msg string }

func (e *unservedError) Error() string { return e.msg }

// errUnserved names model and up to five served ids the workspaces' findings offer instead.
func errUnserved(model string, skipped []agent.Finding) error {
	var near []string
	for _, f := range skipped {
		for _, id := range f.Served {
			if len(near) < 5 && !slices.Contains(near, id) {
				near = append(near, id)
			}
		}
	}
	return &unservedError{fmt.Sprintf("model %s: no signed-in Databricks workspace serves it (nearest served: %s)", model,
		cmp.Or(strings.Join(near, ", "), "none listed"))}
}

// reportPick prints pickArm's pick and the workspaces it skipped on stderr and returns the pick.
func reportPick(p string, live int, skipped []agent.Finding, err error) (string, error) {
	if err == nil {
		note := ""
		if len(skipped) > 0 {
			note = fmt.Sprintf(", %d skipped", len(skipped))
		}
		fmt.Fprintf(os.Stderr, "overlay: databricks:auto picked %s (%d live%s)\n", p, live, note)
	}
	for _, f := range skipped {
		fmt.Fprintf(os.Stderr, "preflight: %s failed %s\n", f.Profile, f)
	}
	return p, err
}

// Seams of the databricks:auto pick; tests stub them, so no credential, quota state or marker is read.
var (
	armFindings = localArmFindings
	armCooling  = func(p *providers.Provider, now time.Time) bool {
		return router.LoadState().CoolingUntil(router.AccountOf(p)).After(now)
	}
	armLive = liveByWorkspace
)

// localArmFindings is p's local preflight (no network): its workspace, then model for agentName when both are known.
func localArmFindings(p *providers.Provider, agentName, model string, now time.Time) []agent.Finding {
	fs := agent.CheckWorkspace(p, router.LoadState(), now)
	if agentName == "" {
		return fs
	}
	model = providers.ConcreteModel(agentName, p, strings.TrimPrefix(model, p.Name+":")) // a family word: p's own member
	if _, failed := agent.CheckModelNoted(agentName, p, model, now, nil); failed != nil {
		fs = append(fs, *failed)
	}
	return fs
}

// liveByWorkspace is each workspace's live load: in-flight markers on its account plus alive records routed to it.
func liveByWorkspace(ps []*providers.Provider) map[string]int {
	snap, routed := inflight.Count(), inflight.LiveRoutes()
	out := map[string]int{}
	for _, p := range ps {
		out[p.Name] = snap.Per[router.AccountOf(p)] + routed[router.JoinCoworker(router.RouteDatabricks, p.Name)]
	}
	return out
}

// armPicks is state/harness/ask/route-auto.json: when databricks:auto last picked each workspace (the tie-break).
type armPicks struct {
	Schema int                  `json:"schema"`
	Picked map[string]time.Time `json:"picked"`
}

const armPicksSchema = 1

var armPicksFormat = schemas.Register(schemas.Format{
	Name: "route-auto", Owner: "internal/agent/ask", Kind: schemas.Cache, Version: armPicksSchema,
	Glob: armPicksFile,
	New:  func() any { return new(armPicks) },
	Keep: "rebuildable cache: it only breaks ties between equally loaded workspaces",
})

func armPicksFile() string { return filepath.Join(paths.StateDir(), "ask", "route-auto.json") }

// usableArms is the workspaces outside tried passing the preflight; a model none serves is dropped with a finding.
func usableArms(reg *providers.Registry, agentName, model string, now time.Time, tried []string) ([]*providers.Provider, []agent.Finding) {
	usable, skipped := armCandidates(reg, agentName, model, now, tried)
	if len(usable) == 0 && model != "" {
		usable, skipped = armCandidates(reg, agentName, "", now, tried)
	}
	return usable, skipped
}

// pickArm picks a workspace outside tried passing the preflight; skipped: one finding per drop.
func pickArm(reg *providers.Registry, agentName, model string, now time.Time, tried ...string) (name string, live int, skipped []agent.Finding, err error) {
	return pickWith(reg, armQuery{agentName: agentName, model: model, tried: tried}, now)
}

// armQuery is one pick: strict refuses a typed model no workspace serves; pending counts this batch's earlier picks.
type armQuery struct {
	agentName, model string
	strict           bool
	tried            []string
	pending          map[string]int
}

// pickWith is pickArm for q: not cooling, fewest live (markers, routed records and q.pending), least recent, name.
func pickWith(reg *providers.Registry, q armQuery, now time.Time) (name string, live int, skipped []agent.Finding, err error) {
	usable, skipped := armCandidates(reg, q.agentName, q.model, now, q.tried)
	if len(usable) == 0 && q.model != "" {
		any, anySkipped := armCandidates(reg, q.agentName, "", now, q.tried)
		if q.strict && len(q.tried) == 0 && len(any) > 0 {
			return "", 0, skipped, errUnserved(q.model, skipped)
		}
		usable, skipped = any, anySkipped
	}
	if len(usable) == 0 {
		return "", 0, skipped, errors.New("overlay databricks:auto: no Databricks workspace passes the preflight (claude-code doctor)")
	}
	cooling := map[string]bool{}
	for _, p := range usable {
		cooling[p.Name] = armCooling(p, now)
	}
	pick := func() error {
		picks := readArmPicks()
		counts := map[string]int{}
		maps.Copy(counts, armLive(usable))
		for n, c := range q.pending {
			counts[n] += c
		}
		best := slices.MinFunc(usable, func(a, b *providers.Provider) int {
			return cmp.Or(cmp.Compare(rank(cooling[a.Name]), rank(cooling[b.Name])), cmp.Compare(counts[a.Name], counts[b.Name]),
				picks.Picked[a.Name].Compare(picks.Picked[b.Name]), cmp.Compare(a.Name, b.Name))
		})
		name, live = best.Name, counts[best.Name]
		picks.Picked[name] = now
		return writeArmPicks(picks)
	}
	if err := paths.WithFileLock(armPicksFile(), pick); name == "" && err != nil { // the stamp is best-effort, the pick is not
		_ = pick()
	}
	if q.pending != nil {
		q.pending[name]++
	}
	return name, live, skipped, nil
}

// rank orders false before true.
func rank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// readArmPicks is the stored stamps; a missing, unreadable or other-version file is an empty table.
func readArmPicks() armPicks {
	picks := armPicks{}
	if raw, err := os.ReadFile(armPicksFile()); err != nil || !schemas.DecodeOK(armPicksFormat, raw, &picks) || picks.Picked == nil {
		picks = armPicks{Picked: map[string]time.Time{}}
	}
	picks.Schema = armPicksSchema
	return picks
}

func writeArmPicks(picks armPicks) error { return paths.WriteJSON(armPicksFile(), picks) }

// errLeave refuses an overlay provider/profile that differs from the inherited confinement provider[:profile].
func errLeave(provider, profile string) error {
	return fmt.Errorf("overlay: a nested dispatch cannot leave %s: drop provider/profile", router.JoinCoworker(provider, profile))
}

// bareArm is the first of profile and the session's profile that names a Databricks row, else "".
func bareArm(ctx context.Context, profile string) string {
	for _, name := range []string{profile, providers.SessionProfileContext(ctx)} {
		if name != "" && providers.ServingRow(name) == nil {
			return name
		}
	}
	return ""
}

// CheckArm refuses a flag taking the dispatch out of an overlay's provider[:profile]; nil outside a confinement.
func CheckArm(ctx context.Context, agentName, profile, provider, model string, direct bool) error {
	arm := router.ArmProfileContext(ctx)
	if arm == "" {
		return confineFlags(ctx, profile, provider, model)
	}
	if err := armFlags(arm, profile, provider, direct); err != nil {
		return err
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("overlay databricks:%s: %w", arm, err)
	}
	spec, err := providers.SplitModelSpec(reg, model)
	if err != nil {
		return err
	}
	if spec.Row != "" && spec.Row != arm {
		return fmt.Errorf("--model: profile %s is outside overlay databricks:%s", spec.Row, arm)
	}
	if !agent.VaultAgentIsClaude(agentName) { // claude has no own-cell hop; the chain's preflight validates every other agent
		return nil
	}
	model = spec.Model
	if _, failed := agent.CheckModelNoted(agentName, reg.Providers[arm], model, time.Now(), nil); failed != nil {
		return fmt.Errorf("--model: not served to %s on databricks:%s", agentName, arm)
	}
	return nil
}

// checkNative refuses --native under any confinement: opencode's own default login is outside every overlay provider.
func checkNative(ctx context.Context, native bool) error {
	if !native {
		return nil
	}
	switch p, prof, err := router.CoworkerContext(ctx); {
	case err != nil:
		return err
	case p != "":
		return fmt.Errorf("--native: outside overlay %s", router.JoinCoworker(p, prof))
	}
	return nil
}

// isArm reports whether profile names the armed workspace in any spelling, or the shared pool (databricks:, auto).
func isArm(profile, arm string) bool {
	row := profile
	for _, sep := range []string{":", "·"} {
		if rest, found := strings.CutPrefix(profile, router.RouteDatabricks+sep); found {
			row = rest
		}
	}
	return row == arm || row == "" || row == "auto"
}

// armFlags refuses --direct, a --profile other than the workspace and a --provider other than databricks.
func armFlags(arm, profile, provider string, direct bool) error {
	switch {
	case direct:
		return fmt.Errorf("--direct: refused inside overlay databricks:%s", arm)
	case profile != "" && !isArm(profile, arm) && profile != agent.LeadSession:
		return fmt.Errorf("--profile %s: outside overlay databricks:%s", profile, arm)
	case provider != "" && provider != router.RouteDatabricks:
		return fmt.Errorf("--provider %s: outside overlay databricks:%s", provider, arm)
	}
	return nil
}

// confineFlags refuses a typed lead outside a non-Databricks confinement: another provider's or pinned profile's row.
func confineFlags(ctx context.Context, profile, provider, model string) error {
	cp, cprof := router.ConfinedContext(ctx)
	typed := strings.Contains(model, ":")
	label := "overlay " + router.JoinCoworker(cp, cprof)
	switch {
	case cp == "":
		return nil
	case provider != "" && provider != cp:
		return fmt.Errorf("--provider %s: outside %s", provider, label)
	case profile == "" && !typed:
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	switch {
	case outsideRow(ctx, reg, profile, cp, cprof):
		return fmt.Errorf("--profile %s: outside %s", profile, label)
	}
	spec, err := providers.SplitModelSpec(reg, model)
	if err != nil {
		return err
	}
	if outsideRow(ctx, reg, spec.Row, cp, cprof) {
		return fmt.Errorf("--model: profile %s is outside %s", spec.Row, label)
	}
	return nil
}

// outsideRow reports name (or the session's profile for "session") as a registry row outside provider cp[:cprof].
func outsideRow(ctx context.Context, reg *providers.Registry, name, cp, cprof string) bool {
	if name == agent.LeadSession {
		name = providers.SessionLeadContext(ctx, reg, true)
	}
	p := reg.Row(name)
	return p != nil && (p.Provider != cp || (cprof != "" && name != cprof))
}

// overlayEgress is ctx with an overlay's remote as the egress host (like --remote); one already set must agree.
func overlayEgress(ctx context.Context, h string) (context.Context, error) {
	if h == "" {
		return ctx, nil
	}
	h, err := remote.ResolveAuto(h)
	if err != nil {
		return ctx, fmt.Errorf("overlay remote: %w", err)
	}
	if err := egress.Valid(h); err != nil {
		return ctx, fmt.Errorf("overlay remote: %w", err)
	}
	if cur := egress.HostContext(ctx); cur != "" && cur != h {
		return ctx, fmt.Errorf("overlay remote %s: requests already leave through %s", h, cur)
	}
	return withValues(ctx, proc.Values{egress.EnvHost: h}), nil
}

// carryOverlay is ctx with the loaded document for --job relaunches as JSON, its remote and confinement resolved.
func carryOverlay(ctx context.Context, ov agentconfig.Overlay) (context.Context, error) {
	if !ov.Routed() && ov.Empty() {
		return ctx, nil
	}
	if ov.Remote != "" {
		ov.Remote = egress.HostContext(ctx)
	}
	// resolved (never auto): the relaunch inherits it as its confinement
	ov.Provider, ov.Profile, _ = router.CoworkerContext(ctx)
	doc, err := json.Marshal(ov) // set holds names and @secret refs: validation refused credential values
	if err != nil {
		return ctx, err
	}
	return withValues(ctx, proc.Values{agentconfig.OverlayVar: string(doc)}), nil
}

// pinLacks reports the agent's Databricks pin with a fresh listing that does not serve model.
func (r *askRun) pinLacks(model string) bool {
	pin, err := agent.PinnedProfile(r.ctx, r.ag)
	reg, rerr := providers.Load()
	if err != nil || rerr != nil || pin == "" || reg.Providers[pin].Preset != providers.PresetDatabricks {
		return false
	}
	p := reg.Providers[pin]
	return agent.FreshModelFinding(r.ag, p, providers.ConcreteModel(r.ag, p, model), time.Now()) != nil
}
