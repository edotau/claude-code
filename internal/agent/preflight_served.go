package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// ServedTTL is how long a cached workspace listing is trusted.
const ServedTTL = 24 * time.Hour

// servedSchema is the served-models cache's schema version; a mismatch is a cache miss.
const servedSchema = 1

var servedFormat = schemas.Register(schemas.Format{
	Name: "served-models", Owner: "internal/agent", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "preflight", "*.json") },
	New:     func() any { return &Served{} },
	Version: servedSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// ServedModel is one serving endpoint the workspace lists.
type ServedModel struct {
	ID    string `json:"id"`
	Task  string `json:"task,omitempty"`
	Ready string `json:"ready,omitempty"`
}

// Served is one Databricks profile's cached endpoint listing (no secrets).
type Served struct {
	Schema    int           `json:"schema"`
	Profile   string        `json:"profile"`
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []ServedModel `json:"models"`
}

// clockSkew is how far into the future a fetched_at may sit before the listing is distrusted.
const clockSkew = 5 * time.Minute

// Fresh reports whether s was fetched within TTL of now; a timestamp beyond clockSkew in the future is stale.
func (s Served) Fresh(now time.Time) bool {
	age := now.Sub(s.FetchedAt)
	return age < ServedTTL && age > -clockSkew
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func servedPath(profile string) string {
	if !nameRE.MatchString(profile) || profile == "." || profile == ".." {
		return ""
	}
	return filepath.Join(paths.StateDir(), "preflight", "served-"+profile+".json")
}

// LoadServed returns profile's cached listing; false when absent, unreadable or another schema.
func LoadServed(profile string) (Served, bool) {
	path := servedPath(profile)
	if path == "" {
		return Served{}, false
	}
	b, err := os.ReadFile(path)
	var s Served
	if err != nil || !schemas.DecodeOK(servedFormat, b, &s) || s.Profile != profile {
		return Served{}, false
	}
	return s, true
}

// SaveServed writes s with mode 0600.
func SaveServed(s Served) error {
	path := servedPath(s.Profile)
	if path == "" {
		return os.ErrInvalid
	}
	s.Schema = servedSchema
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// refreshRetry spaces detached refresh attempts, so an unreachable workspace doesn't spawn one per SessionStart.
const refreshRetry = 15 * time.Minute

// ClaimRefresh takes the detached-refresh slot; false when another attempt started within refreshRetry.
func ClaimRefresh(now time.Time) bool {
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

// RefreshServed lists p's endpoints live and caches them.
func RefreshServed(ctx context.Context, reg *providers.Registry, p *providers.Provider, now time.Time) (Served, error) {
	infos, err := listModels(ctx, reg, p)
	if err != nil {
		return Served{}, err
	}
	s := Served{Profile: p.Name, FetchedAt: now.UTC()}
	for _, m := range infos {
		s.Models = append(s.Models, ServedModel{ID: m.ID, Task: m.Task, Ready: m.Ready})
	}
	return s, SaveServed(s)
}

// Has reports whether the listing serves pin as a READY endpoint; ready is false for a listed but unready one.
func (s Served) Has(p *providers.Provider, pin string) (listed, ready bool) {
	pin = strings.TrimSpace(pin)
	want := p.WireModel(models.Strip1M(pin)) // the spelling the agents request (agent.SpellPin)
	for _, m := range s.Models {
		if m.ID == want {
			return true, m.Ready == "" || m.Ready == "READY"
		}
	}
	return false, false
}
