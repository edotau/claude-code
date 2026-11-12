package ask

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
)

// ApplyRoute exports --route as HARNESS_ROUTE (databricks:<p> also as CLAUDE_PROFILE) for this process and its jobs.
func ApplyRoute(route, profile, agentName, model, fallback string) error {
	if route == "" {
		return nil
	}
	mode, p, err := router.ParseRoute(route)
	if err != nil {
		return err
	}
	if mode == router.RouteDatabricks {
		return applyArm(p, profile, agentName, model, fallback)
	}
	if arm := router.ArmProfile(); arm != "" {
		return errLeaveArm(arm)
	}
	return os.Setenv(router.EnvRoute, mode)
}

// applyArm resolves route databricks:<p> (bare: from profile, else CLAUDE_PROFILE), checks the row and exports it.
func applyArm(p, profile, agentName, model, fallback string) error {
	if p == "" {
		if p = bareArm(profile); p == "" {
			return errors.New("--route databricks needs a workspace: databricks:<profile>")
		}
	}
	inherited, explicit := router.ArmProfile(), p != autoArm
	if !explicit {
		var err error
		if p, err = autoWorkspace(agentName, model); err != nil {
			return err
		}
	}
	if inherited != "" && p != inherited {
		return errLeaveArm(inherited)
	}
	if err := providers.ServingRow(p); err != nil {
		return err
	}
	if explicit && inherited == "" && fallback == router.FallbackNone {
		if err := workspaceRefusal(p); err != nil {
			return err
		}
	}
	if err := os.Setenv(router.EnvRoute, router.RouteDatabricks+":"+p); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, p)
}

// workspaceRefusal is the PreflightError of an explicit workspace failing its local check; nil when it passes.
func workspaceRefusal(name string) error {
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("--route databricks:%s: %w", name, err)
	}
	if fs := agent.CheckWorkspace(reg.Providers[name], router.LoadState(), time.Now()); len(fs) > 0 {
		return &agent.PreflightError{Finding: fs[0]}
	}
	return nil
}

// autoArm is the databricks:auto workspace: the one passing the preflight with the fewest live dispatches (pickArm).
const autoArm = "auto"

// autoWorkspace resolves databricks:auto: the inherited route's workspace inside one, else pickArm's.
func autoWorkspace(agentName, model string) (string, error) {
	if arm := router.ArmProfile(); arm != "" {
		return arm, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("--route databricks:auto: %w", err)
	}
	p, live, skipped, err := pickArm(reg, agentName, model, time.Now())
	if err == nil {
		note := ""
		if len(skipped) > 0 {
			note = fmt.Sprintf(", %d skipped", len(skipped))
		}
		fmt.Fprintf(os.Stderr, "route: databricks:auto picked %s (%d live%s)\n", p, live, note)
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
		out[p.Name] = snap.Per[router.AccountOf(p)] + routed[router.RouteDatabricks+":"+p.Name]
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
		return "", 0, skipped, errors.New("--route databricks:auto: no Databricks workspace passes the preflight (claude-code doctor)")
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

// errLeaveArm refuses a typed --route that differs from the inherited databricks:<arm>.
func errLeaveArm(arm string) error {
	return fmt.Errorf("--route: a nested dispatch cannot leave databricks:%s", arm)
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

// CheckArm refuses a flag that would take the dispatch off route databricks:<p>; nil outside the route.
func CheckArm(agentName, profile, provider, model string, direct bool) error {
	arm := router.ArmProfile()
	if arm == "" {
		return nil
	}
	if err := armFlags(arm, profile, provider, direct); err != nil {
		return err
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("--route databricks:%s: %w", arm, err)
	}
	if prof, _, typed := strings.Cut(model, ":"); typed && reg.Providers[prof] != nil && prof != arm {
		return fmt.Errorf("--model %s: outside --route databricks:%s", model, arm)
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

// armFlags refuses --direct, a --profile other than the workspace and a --provider other than databricks.
func armFlags(arm, profile, provider string, direct bool) error {
	switch {
	case direct:
		return fmt.Errorf("--direct: refused inside --route databricks:%s", arm)
	case profile != "" && profile != arm && profile != agent.LeadSession:
		return fmt.Errorf("--profile %s: outside --route databricks:%s", profile, arm)
	case provider != "" && provider != router.RouteDatabricks:
		return fmt.Errorf("--provider %s: outside --route databricks:%s", provider, arm)
	}
	return nil
}
