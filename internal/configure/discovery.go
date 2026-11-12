package configure

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// discovery.go is the one entry point that lists the models an agent can use on a profile.

// ModelMode is how fresh a read must be; Models turns it into each agent's own parameters.
type ModelMode int

const (
	RecallModels   ModelMode = iota // sealed snapshot, no identity call; a live listing only when none is remembered
	VerifiedModels                  // identity validated, then the snapshot or a live listing; nothing is written
	RefreshModels                   // identity validated, always a live listing, never a stale snapshot
)

// ModelQuery names whose models to list; every other parameter is set per agent inside Models.
type ModelQuery struct {
	Agent   string // a roster name
	Profile string // "" = the agent's pin, else the session profile
	Mode    ModelMode
}

// DiscoveredModel is one usable model.
type DiscoveredModel struct {
	ID string `json:"id"`
}

// ModelResult is one discovery read; Provider, Raw and Snapshot serve callers that probe or seal.
type ModelResult struct {
	Agent   string            `json:"agent"`
	Profile string            `json:"profile,omitempty"`
	Models  []DiscoveredModel `json:"models"`

	Provider *providers.Provider   `json:"-"` // the resolved profile
	Raw      []providers.ModelInfo `json:"-"` // every row the source listed, before the agent filter
	Snapshot *serving.Snapshot     `json:"-"` // Verified and Refresh only: the snapshot a caller may seal
}

// DiscoverModels lists q.Agent's models on q.Profile: one query and one result shape for every agent.
func DiscoverModels(ctx context.Context, q ModelQuery) (ModelResult, error) {
	res := ModelResult{Agent: q.Agent}
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
func profileErr(q ModelQuery, err error) error {
	if q.Agent == agentid.OpenCode && q.Mode != RecallModels && !errors.Is(err, catalog.ErrGateway) {
		return errors.New("cannot resolve profile; check the provider configuration")
	}
	return err
}

// serve is the shared serving read: Recall is catalog.Served, Verified and Refresh are serving.Discover.
func serve(ctx context.Context, res ModelResult, mode ModelMode, dialects []string) (ModelResult, error) {
	p := res.Provider
	if mode != RecallModels {
		snap, err := serving.Discover(ctx, p, mode == RefreshModels)
		if err != nil {
			return res, err
		}
		res.Snapshot, res.Raw = &snap, snap.Models
		res.Models = shape(catalog.UsableIn(p, snap.Models, dialects))
		return res, nil
	}
	infos, _, _, err := catalog.Served(ctx, p)
	if err != nil {
		return res, err
	}
	res.Raw = infos
	res.Models = shape(catalog.UsableIn(p, infos, dialects))
	return res, nil
}

// shape keeps the ids of infos.
func shape(infos []providers.ModelInfo) []DiscoveredModel {
	var out []DiscoveredModel
	for _, m := range infos {
		out = append(out, DiscoveredModel{ID: m.ID})
	}
	return out
}
