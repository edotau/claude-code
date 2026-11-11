// Package catalog lists, probes and discovers a Databricks profile's serving endpoints for every agent's catalog verb.
package catalog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// ErrGateway marks a profile on the AI Gateway wire, which the catalog never lists or probes.
var ErrGateway = errors.New("catalog runs on /serving-endpoints only")

// Profile is agent's catalog profile: flag, else the agent's pin, else the session; nil when none is named.
func Profile(agent, flag string) (*providers.Provider, error) {
	name := cmp.Or(flag, vault.GetAgentRecord(agent).Profile, providers.SessionProfile())
	if name == "" {
		return nil, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	p, err := reg.Get(name)
	if err != nil {
		return nil, err
	}
	if p.IsAIGateway() {
		return nil, fmt.Errorf("profile %s uses the AI Gateway wire; %w", name, ErrGateway)
	}
	return p, nil
}

// Served lists p's endpoints: the sealed snapshot when remembered (hit, with its time), else a live serving listing.
func Served(p *providers.Provider) (infos []providers.ModelInfo, discoveredAt time.Time, hit bool, err error) {
	snap, ok := recallSnapshot(p, func(m []providers.ModelInfo) bool { return len(m) > 0 },
		func(profile string) bool { return SpawnDiscover(profile, false) })
	if ok {
		return snap.Models, snap.DiscoveredAt, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if infos, err = providers.ListModels(ctx, p); err != nil {
		return nil, time.Time{}, false, err
	}
	return infos, time.Now().UTC(), false, nil
}

// byName indexes infos by endpoint name; a repeated name keeps the last row.
func byName(infos []providers.ModelInfo) map[string]providers.ModelInfo {
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out
}

// servedInfo finds id on served under p's model prefix, else as written.
func servedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	if m, ok := served[p.ModelPrefix+id]; ok {
		return m, true
	}
	m, ok := served[id]
	return m, ok
}

// UsableIn keeps infos' READY chat endpoints whose wire on p (providers.WireFor) is one of dialects, sorted by name.
func UsableIn(p *providers.Provider, infos []providers.ModelInfo, dialects []string) []providers.ModelInfo {
	served := byName(infos)
	var out []providers.ModelInfo
	for _, name := range slices.Sorted(maps.Keys(served)) {
		info := served[name]
		if info.Gateway || !info.ReadyChat() {
			continue
		}
		if wire, ok, _ := providers.WireFor(p, name); ok && slices.Contains(dialects, wire) {
			out = append(out, info)
		}
	}
	return out
}

// ID canonicalizes a typed model id: a catalog id via models.Resolve, else the id minus any spelling prefix.
func ID(id string) string {
	id = strings.TrimSpace(models.Strip1M(id))
	if canonical, _, ok := models.Resolve(id); ok {
		return canonical
	}
	return stripSpellingPrefix(id)
}

// stripSpellingPrefix removes the id prefix a catalog spelling rule adds (derived from models.Wire, not listed here).
func stripSpellingPrefix(id string) string {
	for _, rule := range []string{"databricks-serving", "databricks-ai-gateway"} {
		if prefix := strings.TrimSuffix(models.Wire(rule, "x"), "x"); prefix != "" {
			if rest, ok := strings.CutPrefix(id, prefix); ok {
				return rest
			}
		}
	}
	return id
}

// Row is one `<agent> catalog list` line: an endpoint the agent can use and its last probe status ("" = never).
type Row struct {
	Name   string `json:"name"`
	Tested string `json:"tested"`
}

// Rows pairs each endpoint name with the last probe status the shared check cache holds for profile.
func Rows(profile string, names []string) []Row {
	cache, _ := LoadCheckCache(profile)
	var rows []Row
	for _, name := range names {
		rows = append(rows, Row{Name: name, Tested: cache[ID(name)].Status})
	}
	return rows
}

// WriteList renders rows under a "<profile> serving endpoints" title as a NAME / TESTED table, or JSON.
func WriteList(w io.Writer, profile string, rows []Row, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(w, rows)
	}
	fmt.Fprintln(w, profile+" serving endpoints")
	nameW := ColWidth("NAME", len(rows), func(i int) string { return rows[i].Name })
	fmt.Fprintf(w, "%-*s  %s\n", nameW, "NAME", "TESTED")
	for _, r := range rows {
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("%-*s  %s", nameW, r.Name, r.Tested), " "))
	}
	return nil
}

// Seal stores snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func Seal(p *providers.Provider, snapshot serving.Snapshot, verified bool) (sealed bool, err error) {
	if snapshot.CacheHit && !verified {
		return false, nil
	}
	if err := serving.Save(p, snapshot); err != nil {
		return false, err
	}
	return true, nil
}

// SyncSummaryLines are the discovery and verification lines that head a sync report.
func SyncSummaryLines(snapshot serving.Snapshot, verified bool) []string {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	lines := []string{fmt.Sprintf("Valid identity (not endpoint permissions); %s, discovered %s",
		source, snapshot.DiscoveredAt.Format(time.RFC3339))}
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		return append(lines, "Verification: listing only; models are discovered/unverified")
	}
	return append(lines,
		fmt.Sprintf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted",
			label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools),
		"Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}
