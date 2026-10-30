package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// OpencodeProvider resolves the workspace profile: --profile, else the OPENCODE_PROFILE pin.
func OpencodeProvider(name string) (*providers.Registry, *providers.Provider, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, nil, err
	}
	if name == "" {
		if name = providers.Pin(providers.PinOpencodeProfile); name == "" {
			return nil, nil, errors.New("no profile pinned for opencode; pass --profile")
		}
	}
	p, err := reg.Get(name)
	return reg, p, err
}

// OpencodeServed lists the workspace's endpoints by name.
func OpencodeServed(name string) (map[string]providers.ModelInfo, *providers.Provider, error) {
	reg, p, err := OpencodeProvider(name)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return nil, p, err
	}
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out, p, nil
}

func ServedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	if m, ok := served[p.ModelPrefix+id]; ok {
		return m, true
	}
	m, ok := served[id]
	return m, ok
}

// OpencodeCheckTargets resolves the probe set: explicit ids, every served chat endpoint, or the menu.
func OpencodeCheckTargets(ids []string, all bool, served map[string]providers.ModelInfo, p *providers.Provider) []providers.CheckTarget {
	var out []providers.CheckTarget
	if all {
		for name, info := range served {
			if info.Task != "llm/v1/chat" {
				continue
			}
			id := providers.MenuID(name)
			out = append(out, providers.CheckTarget{ID: id, Wire: name, Responses: providers.ResponsesID(id)})
		}
		return out
	}
	if len(ids) == 0 {
		for _, m := range providers.OpencodeMenu() {
			ids = append(ids, m.ID)
		}
	}
	for _, raw := range ids {
		id := providers.MenuID(raw)
		t := providers.CheckTarget{ID: id, Responses: providers.ResponsesID(id)}
		if _, ok := served[p.ModelPrefix+id]; ok {
			t.Wire = p.ModelPrefix + id
		} else if _, ok := served[id]; ok {
			t.Wire = id
		} else {
			t.Missing = true
		}
		out = append(out, t)
	}
	return out
}

// OpencodeSyncModels resolves the profile; discovery validates identity before reading the sealed cache.
func OpencodeSyncModels(profile string, refresh bool) (*providers.Provider, providers.ServingDiscovery, error) {
	_, p, err := OpencodeProvider(profile)
	if err != nil {
		return nil, providers.ServingDiscovery{}, errors.New("cannot resolve profile; check the provider configuration")
	}
	snapshot, err := providers.DiscoverServingModels(context.Background(), p, refresh)
	return p, snapshot, err
}

// OpencodeSyncIDs selects unique, losslessly spellable READY chat endpoint ids in deterministic order.
func OpencodeSyncIDs(infos []providers.ModelInfo) []string {
	var ids []string
	for _, info := range infos {
		if info.Gateway || info.Ready != "READY" || info.Task != "llm/v1/chat" ||
			!strings.HasPrefix(info.ID, models.DatabricksPrefix) {
			continue
		}
		id := providers.MenuID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func OpencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []providers.CheckResult) ([]providers.OpencodeModel, int) {
	failed := make(map[string]bool)
	for _, c := range checks {
		failed[c.ID] = c.Status == "fail" || c.Status == "missing"
	}
	next := slices.Clone(menu)
	skipped := 0
	for _, id := range ids {
		if !slices.ContainsFunc(menu, func(m providers.OpencodeModel) bool { return m.ID == id }) {
			if failed[id] {
				skipped++
				continue
			}
			// MenuAdd canonicalizes its input, so pass the lossless wire name rather than canonicalizing the id twice.
			next = providers.MenuAdd(next, models.Wire("databricks-serving", id), "", nil)
		}
	}
	return next, skipped
}

func OpencodeSyncVerify(p *providers.Provider, snapshot *providers.ServingDiscovery, ids []string, menu []providers.OpencodeModel) error {
	responses := make(map[string]bool, len(menu))
	for _, m := range menu {
		responses[m.ID] = m.Responses
	}
	targets := make([]providers.CheckTarget, 0, len(ids))
	for _, id := range ids {
		r, exists := responses[id]
		if !exists {
			r = models.ResponsesOnly(id)
		}
		targets = append(targets, providers.CheckTarget{ID: id, Wire: models.Wire("databricks-serving", id), Responses: r})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checks, err := providers.ProbeEndpoints(ctx, p, targets,
		providers.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return errors.New("serving verification did not complete; no discovery/menu writes")
	}
	snapshot.Checks = providers.SanitizeServingChecks(checks)
	snapshot.CheckedAt = time.Now().UTC()
	return nil
}
