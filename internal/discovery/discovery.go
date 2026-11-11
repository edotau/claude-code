// Package discovery is the one entry point that lists the models an agent can use on a profile.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// Mode is how fresh a read must be; Models turns it into each agent's own parameters.
type Mode int

const (
	Recall   Mode = iota // sealed snapshot, no identity call; a live listing only when none is remembered
	Verified             // identity validated, then the snapshot or a live listing; nothing is written
	Refresh              // identity validated, always a live listing, never a stale snapshot
)

// Query names whose models to list; every other parameter is set per agent inside Models.
type Query struct {
	Agent   string // a roster name
	Profile string // "" = the agent's pin, else the session profile
	Mode    Mode
}

// Model is one model in the shape every agent returns.
type Model struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	Dialect   string `json:"dialect,omitempty"`
	Responses bool   `json:"responses"`
}

// Result is one discovery read; Provider, Raw and Snapshot serve callers that probe or seal.
type Result struct {
	Agent     string    `json:"agent"`
	Profile   string    `json:"profile,omitempty"`
	Source    string    `json:"source"`
	Models    []Model   `json:"models"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	CacheHit  bool      `json:"cache_hit"`

	Provider *providers.Provider   `json:"-"` // the resolved profile
	Raw      []providers.ModelInfo `json:"-"` // every row the source listed, before the agent filter
	Snapshot *serving.Snapshot     `json:"-"` // Verified and Refresh only: the snapshot a caller may seal
}

// Models lists q.Agent's models on q.Profile: one query and one result shape for every agent.
func Models(ctx context.Context, q Query) (Result, error) {
	res := Result{Agent: q.Agent}
	if !slices.Contains([]string{agentid.Codex, agentid.Copilot, agentid.OpenCode}, q.Agent) {
		return res, fmt.Errorf("discovery: unknown agent %q", q.Agent)
	}
	p, err := catalog.Profile(q.Agent, q.Profile)
	if err != nil {
		return res, profileErr(q, err)
	}
	if p == nil {
		return res, profileErr(q, fmt.Errorf("no profile pinned for %s; pass --profile", q.Agent))
	}
	res.Provider, res.Profile = p, p.Name
	return serve(ctx, res, q.Mode, agent.SchemaFor(q.Agent).CLI.Dialects)
}

// profileErr keeps opencode sync's one-line resolve failure; every other read returns the cause.
func profileErr(q Query, err error) error {
	if q.Agent == agentid.OpenCode && q.Mode != Recall && !errors.Is(err, catalog.ErrGateway) {
		return errors.New("cannot resolve profile; check the provider configuration")
	}
	return err
}

// serve is the shared serving read: Recall is catalog.Served, Verified and Refresh are serving.Discover.
func serve(ctx context.Context, res Result, mode Mode, dialects []string) (Result, error) {
	p := res.Provider
	res.Source, _ = providers.DiscoverySource(p)
	if mode != Recall {
		snap, err := serving.Discover(ctx, p, mode == Refresh)
		if err != nil {
			return res, err
		}
		res.Snapshot, res.Raw, res.FetchedAt, res.CacheHit = &snap, snap.Models, snap.DiscoveredAt, snap.CacheHit
		res.Models = shape(p, catalog.UsableIn(p, snap.Models, dialects))
		return res, nil
	}
	infos, discoveredAt, hit, err := catalog.Served(p)
	if err != nil {
		return res, err
	}
	res.Raw, res.FetchedAt, res.CacheHit = infos, discoveredAt, hit
	res.Models = shape(p, catalog.UsableIn(p, infos, dialects))
	return res, nil
}

// shape renders infos in the common shape; Dialect is the wire p serves each id on.
func shape(p *providers.Provider, infos []providers.ModelInfo) []Model {
	var out []Model
	for _, m := range infos {
		wire, _, _ := providers.WireFor(p, m.ID)
		out = append(out, Model{ID: m.ID, Label: m.Name, Dialect: wire, Responses: providers.ResponsesID(catalog.ID(m.ID))})
	}
	return out
}
