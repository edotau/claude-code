package agent

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/serving"
)

var candidateMemo = paths.NewMemo[routingKey, []catalog.Candidate]("agent.rotation-candidates")
var acceptedMemo = paths.NewMemo[routingKey, acceptedList]("agent.accepted-ids")

type routingKey struct {
	registry *providers.Registry
	listing  *serving.ServedModel
	inputs   [32]byte
}

type acceptedList struct {
	ids   []string
	fresh bool
}

// routingListKey binds reuse to the row, registry, menu, listing, probes and live endpoint marks.
func routingListKey(agentName string, p *providers.Provider, tools bool, now time.Time) (routingKey, error) {
	reg, err := providers.LoadShared()
	if err != nil {
		return routingKey{}, err
	}
	listing, listed := serving.Shared(p.Name)
	gh, ghFresh := githubSnapshot(p, now)
	var snapshot []string
	if p.Provider == "github" {
		snapshot = copilotauth.IDs(p)
	}
	state, err := router.RoutingStateKey(now)
	if err != nil {
		return routingKey{}, err
	}
	dropped, err := catalog.DroppedIDs(p)
	if err != nil {
		return routingKey{}, err
	}
	var probes any
	if tools {
		checks, _ := catalog.LoadCheckCache(p.Name)
		probes = checks
	}
	data, err := json.Marshal([]any{agentName, p.Name, p, providers.OpencodeMenu(), listed, listing.FetchedAt,
		len(listing.Models), listing.Fresh(now), gh, ghFresh, snapshot, state, dropped, tools, probes})
	if err != nil {
		return routingKey{}, err
	}
	key := routingKey{registry: reg, inputs: sha256.Sum256(data)}
	if len(listing.Models) > 0 {
		key.listing = &listing.Models[0]
	}
	return key, nil
}

func memoCandidates(reg *providers.Registry, agentName string, p *providers.Provider, tools bool, now time.Time) []catalog.Candidate {
	key, err := routingListKey(agentName, p, tools, now)
	key.registry = reg
	if err != nil {
		return rotationCandidates(reg, agentName, p, tools, now)
	}
	out, err := candidateMemo.Do(key, func([]catalog.Candidate, error) time.Duration { return 0 }, func() ([]catalog.Candidate, error) {
		return rotationCandidates(reg, agentName, p, tools, now), nil
	})
	if err != nil {
		return rotationCandidates(reg, agentName, p, tools, now)
	}
	return slices.Clone(out)
}
