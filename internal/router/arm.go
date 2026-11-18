package router

import (
	"cmp"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// armPicks is state/harness/router/route-auto.json: when databricks:auto last picked each workspace (the tie-break).
type armPicks struct {
	Schema int                  `json:"schema"`
	Picked map[string]time.Time `json:"picked"`
}

const armPicksSchema = 1

var armPicksFormat = schemas.Register(schemas.Format{
	Name: "route-auto", Owner: "internal/router", Kind: schemas.Cache, Version: armPicksSchema,
	Glob: armPicksFile,
	New:  func() any { return new(armPicks) },
	Keep: "rebuildable cache: it only breaks ties between equally loaded workspaces",
})

func armPicksFile() string { return filepath.Join(paths.StateDir(), "router", "route-auto.json") }

// WorkspaceQuery requires Findings, Unserved and Cooling; nil Live uses in-flight markers and live routes.
type WorkspaceQuery[F any] struct {
	AgentName, Model string
	Strict           bool
	Tried            []string
	Pending          map[string]int
	Findings         func(*providers.Provider, string, string, time.Time) []F
	Unserved         func(string, []F) error
	Cooling          func(*providers.Provider, time.Time) bool
	Live             func([]*providers.Provider) map[string]int
}

// PickWorkspaceAndStamp claims a workspace; only real dispatches should advance the tie-break stamp.
func PickWorkspaceAndStamp[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) (name string, live int, skipped []F, err error) {
	usable, skipped, err := workspaceCandidates(reg, q, now)
	if err != nil {
		return "", 0, skipped, err
	}
	cooling := workspaceCooling(usable, q, now)
	pick := func() error {
		picks := readArmPicks()
		counts := rankWorkspaces(usable, q, cooling, picks)
		name, live = usable[0].Name, counts[usable[0].Name]
		stamp := now // Concurrent callers can acquire the lock out of timestamp order.
		for _, at := range picks.Picked {
			if !stamp.After(at) {
				stamp = at.Add(time.Nanosecond)
			}
		}
		picks.Picked[name] = stamp
		return writeArmPicks(picks)
	}
	if err := paths.WithFileLock(armPicksFile(), pick); name == "" && err != nil { // Stamping is best-effort.
		_ = pick()
	}
	if q.Pending != nil {
		q.Pending[name]++
	}
	return name, live, skipped, nil
}

// RankWorkspaces reads load and stamps without claiming a workspace or changing Pending.
func RankWorkspaces[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) ([]string, []F, error) {
	usable, skipped, err := workspaceCandidates(reg, q, now)
	if err != nil {
		return nil, skipped, err
	}
	rankWorkspaces(usable, q, workspaceCooling(usable, q, now), readArmPicks())
	names := make([]string, len(usable))
	for i, p := range usable {
		names[i] = p.Name
	}
	return names, skipped, nil
}

func workspaceCooling[F any](ps []*providers.Provider, q WorkspaceQuery[F], now time.Time) map[string]bool {
	cooling := map[string]bool{}
	for _, p := range ps {
		cooling[p.Name] = q.Cooling(p, now)
	}
	return cooling
}

func rankWorkspaces[F any](ps []*providers.Provider, q WorkspaceQuery[F], cooling map[string]bool, picks armPicks) map[string]int {
	counts := map[string]int{}
	load := q.Live
	if load == nil {
		load = liveByWorkspace
	}
	maps.Copy(counts, load(ps))
	for n, c := range q.Pending {
		counts[n] += c
	}
	slices.SortFunc(ps, func(a, b *providers.Provider) int {
		return cmp.Or(cmp.Compare(rank(cooling[a.Name]), rank(cooling[b.Name])), cmp.Compare(counts[a.Name], counts[b.Name]),
			picks.Picked[a.Name].Compare(picks.Picked[b.Name]), cmp.Compare(a.Name, b.Name))
	})
	return counts
}

func workspaceCandidates[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) ([]*providers.Provider, []F, error) {
	usable, skipped := armCandidates(reg, q, q.Model, now)
	if len(usable) == 0 && q.Model != "" {
		others, othersSkipped := armCandidates(reg, q, "", now)
		if q.Strict && len(others) > 0 {
			return nil, skipped, q.Unserved(q.Model, skipped)
		}
		usable, skipped = others, othersSkipped
	}
	if len(usable) == 0 {
		return nil, skipped, errors.New("overlay databricks:auto: no Databricks workspace passes the preflight (claude-code doctor)")
	}
	return usable, skipped, nil
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

// armCandidates splits reg's Databricks workspaces (none in tried) by armFindings: usable, skipped.
func armCandidates[F any](reg *providers.Registry, q WorkspaceQuery[F], model string, now time.Time) (usable []*providers.Provider, skipped []F) {
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		p := reg.Providers[n]
		if p.Preset != providers.PresetDatabricks || slices.Contains(q.Tried, n) {
			continue
		}
		if fs := q.Findings(p, q.AgentName, model, now); len(fs) > 0 {
			skipped = append(skipped, fs[0])
			continue
		}
		usable = append(usable, p)
	}
	return usable, skipped
}

// liveByWorkspace is each workspace's live load: in-flight markers on its account plus alive records routed to it.
func liveByWorkspace(ps []*providers.Provider) map[string]int {
	snap, routed := inflight.Count(), inflight.LiveRoutes()
	out := map[string]int{}
	for _, p := range ps {
		out[p.Name] = snap.Per[AccountOf(p)] + routed[JoinCoworker(RouteDatabricks, p.Name)]
	}
	return out
}
