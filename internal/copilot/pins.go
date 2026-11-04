package copilot

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ValidateCopilotPins checks each pin whose effective provider is github-copilot against its live catalog.
// Returns a warning when the catalog itself is unreachable (pin still written by the caller); an unknown
// id is a hard error naming near matches, which the caller must surface before writing anything.
func ValidateCopilotPins(providerFlag string, pins []string) (warn string, err error) {
	fallback := providerFlag
	if fallback == "" {
		fallback = cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile)
	}
	reg, rerr := providers.Load()
	if rerr != nil {
		return fmt.Sprintf("could not verify pins: provider registry unreadable (pin written anyway): %v", rerr), nil
	}
	v := &pinValidator{reg: reg, fallback: fallback, served: map[string][]string{}}
	for _, p := range pins {
		if err := v.check(p); err != nil {
			return "", err
		}
	}
	return v.firstWarn, nil
}

// pinValidator carries ValidateCopilotPins' per-run state: the github catalog is fetched at most once.
type pinValidator struct {
	reg                  *providers.Registry
	fallback             string
	ids                  []string
	fetched, catalogDown bool
	firstWarn            string // the first unverifiable-pin warning; later pins are still checked
	served               map[string][]string
}

// check validates one "<agent>=<spec>" pin; a non-nil error is a hard refusal.
func (v *pinValidator) check(p string) error {
	_, spec, ok := strings.Cut(p, "=")
	if !ok {
		return nil
	}
	name, model := pinTarget(spec, v.fallback)
	if dp := v.reg.Providers[name]; dp != nil && dp.Preset == providers.PresetDatabricks {
		w, err := validateDatabricksCopilotPin(v.reg, name, model, v.served)
		if err != nil {
			return err
		}
		v.firstWarn = cmp.Or(v.firstWarn, w)
		return nil
	}
	gh, err := v.reg.Get(name)
	if err != nil || gh.Provider != "github" {
		return nil
	}
	v.fetchCatalog()
	if v.catalogDown || slices.Contains(v.ids, model) {
		return nil
	}
	return unknownGithubModel(model, gh.Name, v.ids)
}

// fetchCatalog loads the live github-copilot catalog once, remembering a failure as a warning.
func (v *pinValidator) fetchCatalog() {
	if v.fetched {
		return
	}
	v.fetched = true
	var err error
	if v.ids, err = copilotModelIDs(); err != nil {
		v.catalogDown = true
		v.firstWarn = cmp.Or(v.firstWarn, fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err))
	}
}

// pinTarget splits a pin spec into provider and model; a bare model takes fallback as its provider.
func pinTarget(spec, fallback string) (name, model string) {
	name, model, hasProvider := strings.Cut(spec, ":")
	if !hasProvider || model == "" {
		return fallback, spec
	}
	return name, model
}

// unknownGithubModel is the refusal for a model missing from profile's live catalog, naming near matches.
func unknownGithubModel(model, profile string, ids []string) error {
	if near := nearMatches(model, ids); len(near) > 0 {
		return fmt.Errorf("%q is not a github-copilot model; did you mean: %s?", model, strings.Join(near, ", "))
	}
	return fmt.Errorf("%q is not a github-copilot model (run `claude-code models --live --profile %s` to see the catalog)", model, profile)
}

// validateDatabricksCopilotPin checks model against name's serving endpoints; served caches each profile's ids.
func validateDatabricksCopilotPin(
	reg *providers.Registry, name, model string, served map[string][]string,
) (string, error) {
	ids, warn, ok := servedIDs(reg, name, served)
	if !ok {
		return warn, nil
	}
	want := []string{model, models.DatabricksPrefix + model}
	if c, _, ok := models.Resolve(model); ok {
		want = append(want, c)
	}
	for _, id := range ids {
		c, _, _ := models.Resolve(id)
		if slices.Contains(want, id) || (c != "" && slices.Contains(want, c)) {
			return "", nil
		}
	}
	hint := fmt.Sprintf("run `claude-code models --live --profile %s` to see the endpoints", name)
	if near := nearMatches(model, ids); len(near) > 0 {
		return "", fmt.Errorf("%q is not a %s serving endpoint; did you mean: %s? (%s)",
			model, name, strings.Join(near, ", "), hint)
	}
	return "", fmt.Errorf("%q is not a %s serving endpoint (%s)", model, name, hint)
}

// servedIDs lists name's serving endpoint ids once per validation; a failed listing is ok=false, warned once.
func servedIDs(reg *providers.Registry, name string, served map[string][]string) (ids []string, warn string, ok bool) {
	if ids, seen := served[name]; seen {
		return ids, "", ids != nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cat, err := CopilotModelSource(ctx, reg, name)
	cancel()
	if err != nil {
		served[name] = nil // a nil entry marks a failed listing, so later pins don't wait on it again
		return nil, fmt.Sprintf("could not verify against %s serving endpoints (pin written anyway): %v", name, err), false
	}
	ids = make([]string, 0, len(cat.Rows))
	for _, r := range cat.Rows {
		ids = append(ids, r.ID)
	}
	served[name] = ids
	return ids, "", true
}

// nearMatches is a cheap substring match, not full edit distance: enough to catch a typo'd suffix/prefix.
func nearMatches(want string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if strings.Contains(id, want) || strings.Contains(want, id) {
			out = append(out, id)
		}
	}
	return out
}

// copilotModelIDs lists the copilot catalog's model ids (sealed snapshot first).
func copilotModelIDs() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	list, err := Catalog(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, m := range list {
		ids[i] = m.ID
	}
	return ids, nil
}
