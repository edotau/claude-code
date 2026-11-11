package router

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// unservedTTL is how long a model whose endpoint a live call found gone stays out of served-aware picks.
const unservedTTL = time.Hour

// servedListing is profile's fresh cached listing; false when the switch is off, absent or past TTL. A var for tests.
var servedListing = func(profile string, now time.Time) (Served, bool) {
	if !providers.ServedRoutingOn() {
		return Served{}, false
	}
	s, ok := LoadServed(profile)
	return s, ok && s.Fresh(now) && len(s.Models) > 0 // an empty listing proves nothing: read it as cold
}

// servedKey is the spelling a model is marked and looked up under: the one the agents request from p.
func servedKey(p *providers.Provider, model string) string {
	return p.WireModel(models.Strip1M(strings.TrimSpace(model)))
}

// unservedMarked reports whether wire carries an unexpired mark in the profile account's entry e.
func unservedMarked(e Entry, p *providers.Provider, model string, now time.Time) bool {
	at, ok := e.Unserved[servedKey(p, model)]
	return ok && now.Sub(at) < unservedTTL
}

// servedView reads p's listing and unserved marks once into a READY test; fresh is false when no fresh listing exists.
func servedView(p *providers.Provider, now time.Time) (ready func(model string) bool, fresh bool) {
	s, ok := servedListing(p.Name, now)
	if !ok {
		return nil, false
	}
	e := LoadState()[ProviderAccount(p.Name)]
	return func(model string) bool {
		_, ready := s.Has(p, model)
		return ready && !unservedMarked(e, p, model, now)
	}, true
}

// ServedReady reports whether p's fresh listing marks model READY; fresh=false means keep static behavior.
func ServedReady(p *providers.Provider, model string, now time.Time) (ready, fresh bool) {
	isReady, fresh := servedView(p, now)
	if !fresh {
		return false, false
	}
	return isReady(model), true
}

// ServedFirst is the first READY menu entry respelled for p, Responses-only for codex; "" with fresh=true is none.
func ServedFirst(p *providers.Provider, responsesOnly bool, now time.Time) (model string, fresh bool) {
	isReady, fresh := servedView(p, now)
	if !fresh {
		return "", false
	}
	for _, m := range providers.OpencodeMenu() {
		wire := p.WireModel(m.ID)
		if wire != "" && (!responsesOnly || m.Responses) && isReady(m.ID) {
			return wire, true
		}
	}
	return "", true
}

// MarkUnserved remembers that p's endpoint for model is gone, so served-aware picks skip it for unservedTTL.
func MarkUnserved(p *providers.Provider, model string) error {
	now := time.Now()
	return update(func(s State) {
		account := ProviderAccount(p.Name)
		e := s[account]
		next := make(map[string]time.Time, len(e.Unserved)+1)
		for k, at := range e.Unserved {
			if now.Sub(at) < unservedTTL {
				next[k] = at
			}
		}
		next[servedKey(p, model)] = now
		e.Unserved = next
		s[account] = e
	})
}

// servedTTL is how long a cached workspace listing is trusted.
const servedTTL = 24 * time.Hour

// servedSchema is the served-models cache's schema version; a mismatch is a cache miss.
const servedSchema = 1

var servedFormat = schemas.Register(schemas.Format{
	Name: "served-models", Owner: "internal/router", Kind: schemas.Cache,
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
	return !paths.Stale(s.FetchedAt, servedTTL, clockSkew, now)
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func servedPath(profile string) string {
	if !nameRE.MatchString(profile) || profile == "." || profile == ".." {
		return ""
	}
	return filepath.Join(paths.StateDir(), "preflight", "served-"+profile+".json")
}

// servedCache holds each profile's listing per file stamp; LoadServed hands out a copy of its model slice.
var servedCache paths.StampCache[servedRead]

// servedRead is one cached read: ok is false for an absent, mismatched or other-schema file.
type servedRead struct {
	s  Served
	ok bool
}

// LoadServed returns profile's cached listing; false when absent, unreadable or another schema.
func LoadServed(profile string) (Served, bool) {
	path := servedPath(profile)
	if path == "" {
		return Served{}, false
	}
	r, err := servedCache.Get(path, func() (servedRead, error) { return readServed(path, profile) })
	if err != nil || !r.ok {
		return Served{}, false
	}
	r.s.Models = slices.Clone(r.s.Models)
	return r.s, true
}

// readServed decodes profile's listing at path; an absent or invalid file is a miss, a read error is returned.
func readServed(path, profile string) (servedRead, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return servedRead{}, nil
	}
	if err != nil {
		return servedRead{}, err
	}
	var s Served
	if !schemas.DecodeOK(servedFormat, b, &s) || s.Profile != profile {
		return servedRead{}, nil
	}
	return servedRead{s: s, ok: true}, nil
}

// WarmServed saves infos as profile's listing unless the file is newer or infos is empty.
func WarmServed(profile string, infos []providers.ModelInfo, at time.Time) error {
	if len(infos) == 0 {
		return nil
	}
	s := Served{Profile: profile, FetchedAt: at.UTC()}
	for _, m := range infos {
		s.Models = append(s.Models, ServedModel{ID: m.ID, Task: m.Task, Ready: m.Ready})
	}
	// a file dated past clockSkew ahead of at is distrusted and replaced, or it would block every later warm
	return SaveServed(s, func(cur Served) bool { return cur.FetchedAt.After(at) && cur.Fresh(at) })
}

// SaveServed writes s with mode 0600; a keep that accepts the file on disk, read under the write lock, skips the write.
func SaveServed(s Served, keep ...func(cur Served) bool) error {
	path := servedPath(s.Profile)
	if path == "" {
		return os.ErrInvalid
	}
	s.Schema = servedSchema
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // before the lock, which creates it world-readable
		return err
	}
	return paths.WithFileLock(path, func() error {
		for _, k := range keep {
			if cur, _ := readServed(path, s.Profile); cur.ok && k(cur.s) {
				return nil
			}
		}
		return paths.WriteJSON(path, s)
	})
}

// Has reports whether the listing serves pin as a READY endpoint; ready is false for a listed but unready one.
func (s Served) Has(p *providers.Provider, pin string) (listed, ready bool) {
	pin = strings.TrimSpace(pin)
	want := p.WireModel(models.Strip1M(pin)) // the spelling the agents request (providers.SpellPin)
	for _, m := range s.Models {
		if m.ID == want {
			return true, m.Ready == "" || m.Ready == "READY"
		}
	}
	return false, false
}
