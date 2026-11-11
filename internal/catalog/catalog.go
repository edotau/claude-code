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

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/preflight"
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

// Served lists p's endpoints by name: the sealed snapshot when one is remembered, else a live serving listing.
func Served(p *providers.Provider) (map[string]providers.ModelInfo, error) {
	infos, ok := preflight.RecallOrSpawn(p, func(m []providers.ModelInfo) bool { return len(m) > 0 },
		func(profile string) bool { return preflight.SpawnDiscover(profile, false) })
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var err error
		if infos, err = providers.ListModels(ctx, p); err != nil {
			return nil, err
		}
	}
	return byName(infos), nil
}

// byName indexes infos by endpoint name; a repeated name keeps the last row.
func byName(infos []providers.ModelInfo) map[string]providers.ModelInfo {
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out
}

// ServedInfo finds id on served under p's model prefix, else as written.
func ServedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	if m, ok := served[p.ModelPrefix+id]; ok {
		return m, true
	}
	m, ok := served[id]
	return m, ok
}

// Usable keeps served's READY chat endpoints whose wire on p (providers.WireFor) is one of dialects, by name.
func Usable(p *providers.Provider, served map[string]providers.ModelInfo, dialects []string) []providers.ModelInfo {
	var out []providers.ModelInfo
	for _, name := range slices.Sorted(maps.Keys(served)) {
		info := served[name]
		if info.Gateway || info.Ready != "READY" || info.Task != "llm/v1/chat" {
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

// List is p's endpoints usable on dialects, each with the last probe status the shared check cache holds.
func List(p *providers.Provider, dialects []string) ([]Row, error) {
	served, err := Served(p)
	if err != nil {
		return nil, err
	}
	cache, _ := LoadCheckCache(p.Name)
	var rows []Row
	for _, info := range Usable(p, served, dialects) {
		rows = append(rows, Row{Name: info.ID, Tested: cache[ID(info.ID)].Status})
	}
	return rows, nil
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
