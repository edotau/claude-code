package serving

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// CacheTTL is how long a cached workspace listing is trusted.
const CacheTTL = 24 * time.Hour

const (
	servedSchema = 1
	clockSkew    = 5 * time.Minute
)

var servedFormat = schemas.Register(schemas.Format{
	Name: "served-models", Owner: "internal/serving", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "preflight", "*.json") },
	New:     func() any { return &Served{} },
	Version: servedSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})

// EndpointFacts are one workspace endpoint's listing facts; an empty or zero field is unknown.
type EndpointFacts struct {
	APITypes        []string
	Context, Output int
}

// ServedModel is one serving endpoint the workspace lists.
type ServedModel struct {
	ID         string   `json:"id"`
	Task       string   `json:"task,omitempty"`
	Ready      string   `json:"ready,omitempty"`
	APITypes   []string `json:"api_types,omitempty"`
	ModelClass string   `json:"model_class,omitempty"`
	Context    int      `json:"context,omitempty"`
	Output     int      `json:"output,omitempty"`
}

// IsReady reports whether the endpoint serves: an unset state counts as READY.
func (m ServedModel) IsReady() bool { return m.Ready == "" || m.Ready == "READY" }

// Served is one Databricks profile's cached endpoint listing (no secrets).
type Served struct {
	Schema    int           `json:"schema"`
	Profile   string        `json:"profile"`
	FetchedAt time.Time     `json:"fetched_at"`
	Models    []ServedModel `json:"models"`
}

// Fresh reports whether s was fetched within CacheTTL of now; a timestamp beyond clockSkew in the future is stale.
func (s Served) Fresh(now time.Time) bool {
	return !paths.Stale(s.FetchedAt, CacheTTL, clockSkew, now)
}

// WireNamer maps a canonical or plain model id to a profile's wire id (satisfied by *providers.Provider).
type WireNamer interface {
	WireModel(string) string
}

// FindWire matches wire (or databricks-<wire>) in s.Models; false when unlisted.
func (s Served) FindWire(wire string) (ServedModel, bool) {
	for _, m := range s.Models {
		if m.ID == wire || m.ID == models.DatabricksPrefix+wire {
			return m, true
		}
	}
	return ServedModel{}, false
}

// Find is the listing's entry serving pin (the spelling the agents request); false when unlisted.
func (s Served) Find(p WireNamer, pin string) (ServedModel, bool) {
	return s.FindWire(p.WireModel(models.Strip1M(strings.TrimSpace(pin))))
}

// Has reports whether the listing serves pin as a READY endpoint; ready is false for a listed but unready one.
func (s Served) Has(p WireNamer, pin string) (listed, ready bool) {
	m, ok := s.Find(p, pin)
	return ok, ok && m.IsReady()
}

var nameRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]+$`) })

// Path is profile's served-models cache path, or "" if profile is unsafe.
func Path(profile string) string {
	if !nameRE().MatchString(profile) || profile == "." || profile == ".." {
		return ""
	}
	return filepath.Join(paths.StateDir(), "preflight", "served-"+profile+".json")
}

type servedRead struct {
	s  Served
	ok bool
}

var servedCache = paths.NewStampCache[servedRead]("serving.served")

// Shared returns profile's cached listing without copying Models; callers must treat Models as read-only.
func Shared(profile string) (Served, bool) {
	path := Path(profile)
	if path == "" {
		return Served{}, false
	}
	r, err := servedCache.Get(path, func() (servedRead, error) { return readServed(path, profile) })
	if err != nil || !r.ok {
		return Served{}, false
	}
	return r.s, true
}

// Load returns profile's cached listing; false when absent, unreadable or another schema.
func Load(profile string) (Served, bool) {
	s, ok := Shared(profile)
	s.Models = slices.Clone(s.Models)
	return s, ok
}

// FreshShared returns Shared when fresh and non-empty; callers must treat Models as read-only.
func FreshShared(profile string, now time.Time) (Served, bool) {
	s, ok := Shared(profile)
	return s, ok && s.Fresh(now) && len(s.Models) > 0
}

// Fresh returns profile's cached listing when fresh and non-empty; Models is a copy.
func Fresh(profile string, now time.Time) (Served, bool) {
	s, ok := FreshShared(profile, now)
	s.Models = slices.Clone(s.Models)
	return s, ok
}

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

// Save writes s with mode 0600; a keep predicate matching the on-disk file under lock skips the write.
func Save(s Served, keep ...func(cur Served) bool) error {
	path := Path(s.Profile)
	if path == "" {
		return os.ErrInvalid
	}
	s.Schema = servedSchema
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(path, func() error {
		for _, k := range keep {
			if cur, _ := readServed(path, s.Profile); cur.ok && k(cur.s) {
				return nil
			}
		}
		return paths.WriteJSONNoSync(path, s)
	})
}

var (
	factsMu       sync.RWMutex
	factsOverride func(profile, wire string) (EndpointFacts, bool)
)

// SetFactsForTest overrides Facts for the duration of t and restores it on cleanup.
func SetFactsForTest(t interface {
	Helper()
	Cleanup(func())
}, fn func(profile, wire string) (EndpointFacts, bool)) {
	t.Helper()
	factsMu.Lock()
	prev := factsOverride
	factsOverride = fn
	factsMu.Unlock()
	t.Cleanup(func() {
		factsMu.Lock()
		factsOverride = prev
		factsMu.Unlock()
	})
}

// Facts reads wire's entry in profile's fresh served listing directly from the cache.
func Facts(profile, wire string) (EndpointFacts, bool) {
	factsMu.RLock()
	fn := factsOverride
	factsMu.RUnlock()
	if fn != nil {
		return fn(profile, wire)
	}
	s, ok := FreshShared(profile, time.Now())
	if !ok {
		return EndpointFacts{}, false
	}
	m, ok := s.FindWire(strings.TrimSpace(wire))
	return EndpointFacts{APITypes: m.APITypes, Context: m.Context, Output: m.Output}, ok
}
