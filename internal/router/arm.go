package router

import (
	"cmp"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	Extra            []*providers.Provider // rows ranked beside the Databricks workspaces (claude's subscription row)
	Key              string                // stamp namespace in route-auto.json; "" is the shared workspace tie-break
}

// RowRotationKey is the route-auto.json namespace of agent's own row rotation, apart from databricks:auto's stamps.
func RowRotationKey(agentName string) string { return agentName + "·rows" }

// RowPicks is when agent's row rotation last picked each row, keyed by row name.
func RowPicks(agentName string) map[string]time.Time {
	out := map[string]time.Time{}
	prefix := RowRotationKey(agentName) + "/"
	for k, at := range readArmPicks().Picked {
		if row, ok := strings.CutPrefix(k, prefix); ok {
			out[row] = at
		}
	}
	return out
}

func (q WorkspaceQuery[F]) stampKey(name string) string {
	if q.Key == "" {
		return name
	}
	return q.Key + "/" + name
}

// PickWorkspaceAndStamp claims a workspace; only real dispatches should advance the tie-break stamp.
func PickWorkspaceAndStamp[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) (name string, live int, skipped []F, err error) {
	order, live, skipped, err := pickAndStamp(reg, q, now)
	if err != nil {
		return "", 0, skipped, err
	}
	return order[0], live, skipped, nil
}

// PickRowsAndStamp is PickWorkspaceAndStamp returning every usable row in rank order, the claimed one first.
func PickRowsAndStamp[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) ([]string, []F, error) {
	order, _, skipped, err := pickAndStamp(reg, q, now)
	return order, skipped, err
}

func pickAndStamp[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) (order []string, live int, skipped []F, err error) {
	usable, skipped, err := workspaceCandidates(reg, q, now)
	if err != nil {
		return nil, 0, skipped, err
	}
	cooling := workspaceCooling(usable, q, now)
	load := workspaceLoad(usable, q) // the marker and record scans stay out of the stamp lock
	pick := func() error {
		picks := readArmPicks()
		counts := rankWorkspaces(usable, q, cooling, load, picks)
		order, live = names(usable), counts[usable[0].Name]
		picks.Picked[q.stampKey(order[0])] = newestStamp(picks, now)
		return writeArmPicks(picks)
	}
	if err := paths.WithFileLock(armPicksFile(), pick); len(order) == 0 && err != nil { // Stamping is best-effort.
		_ = pick()
	}
	if q.Pending != nil {
		q.Pending[order[0]]++
	}
	return order, live, skipped, nil
}

// newestStamp is now, nudged past every stored stamp: concurrent callers can acquire the lock out of timestamp order.
func newestStamp(picks armPicks, now time.Time) time.Time {
	stamp := now
	for _, at := range picks.Picked {
		if !stamp.After(at) {
			stamp = at.Add(time.Nanosecond)
		}
	}
	return stamp
}

// StampRow records that agentName's row rotation ran row; best-effort like the pick's own stamp.
func StampRow(agentName, row string, now time.Time) {
	_ = paths.WithFileLock(armPicksFile(), func() error {
		picks := readArmPicks()
		picks.Picked[RowRotationKey(agentName)+"/"+row] = newestStamp(picks, now)
		return writeArmPicks(picks)
	})
}

func names(ps []*providers.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// RankWorkspaces reads load and stamps without claiming a workspace or changing Pending.
func RankWorkspaces[F any](reg *providers.Registry, q WorkspaceQuery[F], now time.Time) ([]string, []F, error) {
	usable, skipped, err := workspaceCandidates(reg, q, now)
	if err != nil {
		return nil, skipped, err
	}
	rankWorkspaces(usable, q, workspaceCooling(usable, q, now), workspaceLoad(usable, q), readArmPicks())
	return names(usable), skipped, nil
}

func workspaceCooling[F any](ps []*providers.Provider, q WorkspaceQuery[F], now time.Time) map[string]bool {
	cooling := map[string]bool{}
	for _, p := range ps {
		cooling[p.Name] = q.Cooling(p, now)
	}
	return cooling
}

func workspaceLoad[F any](ps []*providers.Provider, q WorkspaceQuery[F]) map[string]int {
	counts := map[string]int{}
	load := q.Live
	if load == nil {
		load = liveByWorkspace
	}
	maps.Copy(counts, load(ps))
	for n, c := range q.Pending {
		counts[n] += c
	}
	return counts
}

func rankWorkspaces[F any](ps []*providers.Provider, q WorkspaceQuery[F], cooling map[string]bool, counts map[string]int, picks armPicks) map[string]int {
	slices.SortFunc(ps, func(a, b *providers.Provider) int {
		return cmp.Or(cmp.Compare(rank(cooling[a.Name]), rank(cooling[b.Name])), cmp.Compare(counts[a.Name], counts[b.Name]),
			picks.Picked[q.stampKey(a.Name)].Compare(picks.Picked[q.stampKey(b.Name)]), cmp.Compare(a.Name, b.Name))
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

// armCandidates splits reg's Databricks workspaces plus q.Extra (none in tried) by armFindings: usable, skipped.
func armCandidates[F any](reg *providers.Registry, q WorkspaceQuery[F], model string, now time.Time) (usable []*providers.Provider, skipped []F) {
	rows := []*providers.Provider{}
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		if p := reg.Providers[n]; p.Preset == providers.PresetDatabricks {
			rows = append(rows, p)
		}
	}
	for _, p := range append(rows, q.Extra...) {
		n := p.Name
		if slices.Contains(q.Tried, n) {
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
	var routed map[string]int
	done := make(chan struct{})
	go func() {
		defer close(done)
		routed = inflight.LiveRoutes()
	}()
	snap := inflight.Count()
	<-done
	out := map[string]int{}
	for _, p := range ps {
		out[p.Name] = snap.Per[AccountOf(p)] + routed[JoinCoworker(RouteDatabricks, p.Name)]
	}
	return out
}
