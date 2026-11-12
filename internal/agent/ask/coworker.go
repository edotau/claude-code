package ask

import (
	"cmp"
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
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
)

// ApplyCoworker exports confinement (Overlay.Confinement) as router.EnvCoworker, databricks:<p> also as CLAUDE_PROFILE.
func ApplyCoworker(value, profile, agentName, model, fallback string) error {
	inherited := strings.TrimSpace(os.Getenv(router.EnvCoworker))
	ip, iprof, err := router.ParseCoworker(inherited)
	if err != nil {
		return fmt.Errorf("%s: %w", router.EnvCoworker, err)
	}
	value, src := coworkerSource(value, inherited, ip, iprof)
	provider, p, err := router.ParseCoworker(value)
	switch {
	case err != nil:
		return fmt.Errorf("%s%w", src, err)
	case provider == "":
		return applyInherited(ip, iprof, agentName)
	}
	if err := checkLeave(ip, iprof, provider, p); err != nil {
		return err
	}
	if provider != router.RouteDatabricks {
		return applyProvider(provider, cmp.Or(p, iprof), agentName)
	}
	if err := applyArm(p, profile, agentName, model, fallback); err != nil {
		return armErr(src, err)
	}
	return nil
}

// applyInherited re-checks an inherited non-Databricks confinement for agentName; a workspace was applied where set.
func applyInherited(ip, iprof, agentName string) error {
	if ip == "" || ip == router.RouteDatabricks {
		return nil
	}
	return applyProvider(ip, iprof, agentName)
}

var errNoWorkspace = errors.New("overlay provider databricks needs a profile: a workspace or auto")

// armErr prefixes applyArm's error with its source; an env or vault value has no overlay to blame.
func armErr(src string, err error) error {
	if src != "" && errors.Is(err, errNoWorkspace) {
		return fmt.Errorf("%sdatabricks needs a workspace: databricks:<profile> or databricks:auto", src)
	}
	return fmt.Errorf("%s%w", src, err)
}

// coworkerSource is the value ApplyCoworker applies and its error prefix: overlay > bare inherited databricks > vault.
func coworkerSource(value, inherited, ip, iprof string) (string, string) {
	switch {
	case value != "":
		return value, ""
	case inherited == "":
		return providers.CustomEnv()[router.EnvCoworker], "vault env " + router.EnvCoworker + ": "
	case ip == router.RouteDatabricks && iprof == "": // bare: arms a workspace as the vault value does
		return inherited, router.EnvCoworker + ": "
	}
	return "", ""
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

// applyProvider checks profile p and agentName against a non-Databricks provider, then exports the confinement.
func applyProvider(provider, p, agentName string) error {
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("overlay provider %s: %w", provider, err)
	}
	if row := reg.Providers[p]; p != "" && (row == nil || row.Provider != provider) {
		return fmt.Errorf("overlay profile: not a profile of %s", provider) // never echo the document's value
	}
	if err := canUse(reg, agentName, provider); err != nil {
		return err
	}
	return os.Setenv(router.EnvCoworker, router.JoinCoworker(provider, p))
}

// canUse refuses a table agent with no account on provider; another agent name (a batch's "") passes.
func canUse(reg *providers.Registry, agentName, provider string) error {
	if slices.Contains(router.TableAgents, agentName) && !router.CanUse(reg, agentName, provider) {
		return fmt.Errorf("overlay provider %s: %s has no %s account", provider, agentName, provider)
	}
	return nil
}

// confinedAgent is canUse for agentName against the non-Databricks confinement in the env; nil outside one.
func confinedAgent(agentName string) error {
	cp, _ := router.Confined()
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
func confineLine(agentName string, req agent.RunOptions) (agent.RunOptions, error) {
	err := cmp.Or(checkNative(req.Native), confinedAgent(agentName), CheckArm(agentName, req.Provider, "", req.Model, false))
	if err != nil {
		return req, err // the line's own profile and model, checked like the typed flags
	}
	profile, provider := router.ConfineLead(agentName, req.Provider, "", req.Model)
	if req.Lead != "" || (profile == req.Provider && provider == "") {
		return req, nil
	}
	return lineLead(agentName, req, profile, provider)
}

// lineLead sets a batch line's lead from the confinement's profile or provider (agent.LeadFor).
func lineLead(agentName string, req agent.RunOptions, profile, provider string) (agent.RunOptions, error) {
	q := agent.RouteReq{Agent: agentName, Model: req.Model, Profile: profile, Provider: provider, Fallback: req.Fallback}
	lead, pin, err := agent.LeadFor(q)
	switch {
	case err != nil && !pin:
		return req, err
	case lead == "" || agentName == "claude": // claude reads Provider, never Lead
		req.Provider = cmp.Or(lead, profile)
	default:
		req.Lead, req.Provider = lead, ""
	}
	return req, nil
}

// applyArm resolves databricks profile <p> (bare: from profile, else CLAUDE_PROFILE), checks the row and exports it.
func applyArm(p, profile, agentName, model, fallback string) error {
	p, explicit, err := armWorkspace(p, profile, agentName, model)
	if err != nil {
		return err
	}
	if err := checkArmRow(p, explicit, fallback); err != nil {
		return err
	}
	if err := os.Setenv(router.EnvCoworker, router.JoinCoworker(router.RouteDatabricks, p)); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, p)
}

// armWorkspace is the workspace databricks:<p> names (bare: bareArm, auto: autoWorkspace); explicit: neither auto.
func armWorkspace(p, profile, agentName, model string) (string, bool, error) {
	if p == "" {
		if p = bareArm(profile); p == "" {
			return "", false, errNoWorkspace
		}
	}
	if p != autoArm {
		return p, true, nil
	}
	p, err := autoWorkspace(agentName, model)
	return p, false, err
}

// checkArmRow refuses leaving the inherited workspace, a non-serving row, and an explicit one failing its preflight.
func checkArmRow(p string, explicit bool, fallback string) error {
	inherited := router.ArmProfile()
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

// autoArm is the databricks:auto workspace: the one passing the preflight with the fewest live dispatches (pickArm).
const autoArm = "auto"

// autoWorkspace resolves databricks:auto: the inherited confinement's workspace inside one, else pickArm's.
func autoWorkspace(agentName, model string) (string, error) {
	if arm := router.ArmProfile(); arm != "" {
		return arm, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("overlay databricks:auto: %w", err)
	}
	p, live, skipped, err := pickArm(reg, agentName, model, time.Now())
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
	if _, failed := agent.CheckModelNoted(agentName, p, strings.TrimPrefix(model, p.Name+":"), now, nil); failed != nil {
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

// pickArm picks a workspace passing the preflight (a model none serves is dropped); skipped: one finding per drop.
func pickArm(reg *providers.Registry, agentName, model string, now time.Time) (name string, live int, skipped []agent.Finding, err error) {
	usable, skipped := armCandidates(reg, agentName, model, now)
	if len(usable) == 0 && model != "" {
		usable, skipped = armCandidates(reg, agentName, "", now)
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
		counts := armLive(usable)
		best := slices.MinFunc(usable, func(a, b *providers.Provider) int { // not cooling, fewest live, least recent, name
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
	return name, live, skipped, nil
}

// armCandidates splits reg's Databricks workspaces (no AI Gateway row) into those passing armFindings and the rest.
func armCandidates(reg *providers.Registry, agentName, model string, now time.Time) (usable []*providers.Provider, skipped []agent.Finding) {
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		p := reg.Providers[n]
		if p.Preset != providers.PresetDatabricks || p.IsAIGateway() {
			continue
		}
		if fs := armFindings(p, agentName, model, now); len(fs) > 0 {
			skipped = append(skipped, fs[0])
			continue
		}
		usable = append(usable, p)
	}
	return usable, skipped
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

// bareArm is the first of profile and CLAUDE_PROFILE that names a Databricks row, else "".
func bareArm(profile string) string {
	for _, name := range []string{profile, os.Getenv(providers.EnvSessionProfile)} {
		if name != "" && providers.ServingRow(name) == nil {
			return name
		}
	}
	return ""
}

// CheckArm refuses a flag taking the dispatch out of an overlay's provider[:profile]; nil outside a confinement.
func CheckArm(agentName, profile, provider, model string, direct bool) error {
	arm := router.ArmProfile()
	if arm == "" {
		return confineFlags(profile, provider, model)
	}
	if err := armFlags(arm, profile, provider, direct); err != nil {
		return err
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("overlay databricks:%s: %w", arm, err)
	}
	if prof, _, typed := strings.Cut(model, ":"); typed && reg.Providers[prof] != nil && prof != arm {
		return fmt.Errorf("--model %s: outside overlay databricks:%s", model, arm)
	}
	if agentName != "claude" { // claude has no own-cell hop; the chain's preflight validates every other agent
		return nil
	}
	model = strings.TrimPrefix(model, arm+":")
	if _, failed := agent.CheckModelNoted(agentName, reg.Providers[arm], model, time.Now(), nil); failed != nil {
		return fmt.Errorf("--model %s: not served to %s on databricks:%s", model, agentName, arm)
	}
	return nil
}

// checkNative refuses --native under any confinement: opencode's own default login is outside every overlay provider.
func checkNative(native bool) error {
	if !native {
		return nil
	}
	switch p, prof, err := router.Coworker(); {
	case err != nil:
		return fmt.Errorf("%s: %w", router.EnvCoworker, err)
	case p != "":
		return fmt.Errorf("--native: outside overlay %s", router.JoinCoworker(p, prof))
	}
	return nil
}

// armFlags refuses --direct, a --profile other than the workspace and a --provider other than databricks.
func armFlags(arm, profile, provider string, direct bool) error {
	switch {
	case direct:
		return fmt.Errorf("--direct: refused inside overlay databricks:%s", arm)
	case profile != "" && profile != arm && profile != agent.LeadSession:
		return fmt.Errorf("--profile %s: outside overlay databricks:%s", profile, arm)
	case provider != "" && provider != router.RouteDatabricks:
		return fmt.Errorf("--provider %s: outside overlay databricks:%s", provider, arm)
	}
	return nil
}

// confineFlags refuses a typed lead outside a non-Databricks confinement: another provider's or pinned profile's row.
func confineFlags(profile, provider, model string) error {
	cp, cprof := router.Confined()
	prof, _, typed := strings.Cut(model, ":")
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
	case outsideRow(reg, profile, cp, cprof):
		return fmt.Errorf("--profile %s: outside %s", profile, label)
	case typed && outsideRow(reg, prof, cp, cprof):
		return fmt.Errorf("--model %s: outside %s", model, label)
	}
	return nil
}

// outsideRow reports name (or the session's profile for "session") as a registry row outside provider cp[:cprof].
func outsideRow(reg *providers.Registry, name, cp, cprof string) bool {
	if name == agent.LeadSession {
		name = providers.SessionLead(reg, true)
	}
	p := reg.Row(name)
	return p != nil && (p.Provider != cp || (cprof != "" && name != cprof))
}

// overlayEgress exports an overlay's remote as the egress host, like the global --remote; one already set must agree.
func overlayEgress(h string) error {
	if h == "" {
		return nil
	}
	h, err := remote.ResolveAuto(h)
	if err != nil {
		return fmt.Errorf("overlay remote: %w", err)
	}
	if err := egress.Valid(h); err != nil {
		return fmt.Errorf("overlay remote: %w", err)
	}
	if cur := egress.Host(); cur != "" && cur != h {
		return fmt.Errorf("overlay remote %s: requests already leave through %s", h, cur)
	}
	return os.Setenv(egress.EnvHost, h)
}

// carryOverlay re-exports the loaded document for --job relaunches as JSON, its remote already resolved (never auto).
func carryOverlay(ov agentconfig.Overlay) error {
	if !ov.Routed() && ov.Empty() {
		return nil
	}
	if ov.Remote != "" {
		ov.Remote = egress.Host()
	}
	doc, err := json.Marshal(ov) // set holds names and @secret refs: validation refused credential values
	if err != nil {
		return err
	}
	return os.Setenv(agentconfig.OverlayVar, string(doc))
}
