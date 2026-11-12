package ask

import (
	"cmp"
	"errors"
	"fmt"
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
func ApplyRoute(route, profile string) error {
	if route == "" {
		return nil
	}
	mode, p, err := router.ParseRoute(route)
	if err != nil {
		return err
	}
	if mode == router.RouteDatabricks {
		return applyArm(p, profile)
	}
	if arm := router.ArmProfile(); arm != "" {
		return errLeaveArm(arm)
	}
	return os.Setenv(router.EnvRoute, mode)
}

// applyArm resolves route databricks:<p> (bare: from profile, else CLAUDE_PROFILE), checks the row and exports it.
func applyArm(p, profile string) error {
	if p == "" {
		if p = bareArm(profile); p == "" {
			return errors.New("--route databricks needs a workspace: databricks:<profile>")
		}
	}
	if p == autoArm {
		var err error
		if p, err = autoWorkspace(); err != nil {
			return err
		}
	}
	if arm := router.ArmProfile(); arm != "" && p != arm {
		return errLeaveArm(arm)
	}
	if err := providers.ServingRow(p); err != nil {
		return err
	}
	if err := os.Setenv(router.EnvRoute, router.RouteDatabricks+":"+p); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, p)
}

// autoArm is the databricks:auto workspace: the usable one with the fewest live dispatches (pickArm).
const autoArm = "auto"

// autoWorkspace resolves databricks:auto: the inherited route's workspace inside one, else pickArm's.
func autoWorkspace() (string, error) {
	if arm := router.ArmProfile(); arm != "" {
		return arm, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("--route databricks:auto: %w", err)
	}
	p, live, err := pickArm(reg, time.Now())
	if err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "route: databricks:auto picked %s (%d live)\n", p, live)
	return p, nil
}

// Seams of the databricks:auto pick; tests stub both, so no credential resolves and no marker is read.
var (
	armUsable = func(p *providers.Provider) bool { return !p.IsAIGateway() && p.DatabricksUsable() }
	armLive   = liveByWorkspace
)

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

// pickArm picks the usable Databricks workspace with the fewest live dispatches (tie: least recent pick, then name).
func pickArm(reg *providers.Registry, now time.Time) (name string, live int, err error) {
	var usable []*providers.Provider
	for _, p := range reg.Providers {
		if p.Preset == providers.PresetDatabricks && armUsable(p) {
			usable = append(usable, p)
		}
	}
	if len(usable) == 0 {
		return "", 0, errors.New("--route databricks:auto: no Databricks workspace with a usable credential (claude-code doctor)")
	}
	pick := func() error {
		picks := readArmPicks()
		counts := armLive(usable)
		best := slices.MinFunc(usable, func(a, b *providers.Provider) int {
			return cmp.Or(cmp.Compare(counts[a.Name], counts[b.Name]), picks.Picked[a.Name].Compare(picks.Picked[b.Name]),
				cmp.Compare(a.Name, b.Name))
		})
		name, live = best.Name, counts[best.Name]
		picks.Picked[name] = now
		return writeArmPicks(picks)
	}
	if err := paths.WithFileLock(armPicksFile(), pick); name == "" && err != nil { // the stamp is best-effort, the pick is not
		_ = pick()
	}
	return name, live, nil
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
	return armModel(reg.Providers[arm], agentName, strings.TrimPrefix(model, arm+":"))
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

// armModel refuses an explicit model id the workspace does not serve agentName; aliases and discovery pass.
func armModel(p *providers.Provider, agentName, model string) error {
	if model == "" || agentName == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, model) {
		return nil
	}
	served := providers.SpellPin(p, model) != ""
	if agentName != "claude" { // Serves covers external agents only
		_, why := agent.ProfileServes(agentName, p, model)
		served = why == ""
	}
	if !served {
		return fmt.Errorf("--model %s: not served to %s on databricks:%s", model, agentName, p.Name)
	}
	return nil
}
