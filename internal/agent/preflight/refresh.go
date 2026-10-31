package preflight

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// refreshRetry spaces detached refresh attempts, so an unreachable workspace doesn't spawn one per SessionStart.
const refreshRetry = 15 * time.Minute

// Claim takes the detached-refresh slot; false when another attempt started within refreshRetry.
func Claim(now time.Time) bool {
	path := filepath.Join(paths.StateDir(), "preflight", "refresh-attempt")
	if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) < refreshRetry {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = os.WriteFile(path, nil, 0o600) // best effort: an unwritable stamp must not stop the refresh
	}
	return true
}

// listModels is the live workspace listing; a seam for tests.
var listModels = providers.ListModels

// Fetch lists p's endpoints live and caches them.
func Fetch(ctx context.Context, reg *providers.Registry, p *providers.Provider, now time.Time) (agent.Served, error) {
	infos, err := listModels(ctx, reg, p)
	if err != nil {
		return agent.Served{}, err
	}
	s := agent.Served{Profile: p.Name, FetchedAt: now.UTC()}
	for _, m := range infos {
		s.Models = append(s.Models, agent.ServedModel{ID: m.ID, Task: m.Task, Ready: m.Ready})
	}
	return s, agent.SaveServed(s)
}
