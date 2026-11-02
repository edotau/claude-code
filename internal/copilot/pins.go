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
		return "", nil
	}
	var ids []string
	var fetched, catalogDown bool
	var firstWarn string // the first unverifiable-pin warning; later pins are still checked
	served := map[string][]string{}
	for _, p := range pins {
		_, spec, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		name, model := pinTarget(spec, fallback)
		if dp := reg.Providers[name]; dp != nil && dp.Preset == providers.PresetDatabricks {
			w, derr := validateDatabricksCopilotPin(reg, name, model, served)
			if derr != nil {
				return "", derr
			}
			firstWarn = cmp.Or(firstWarn, w)
			continue
		}
		gh, gerr := reg.Get(name)
		if gerr != nil || gh.Provider != "github" {
			continue
		}
		if !fetched {
			fetched = true
			ids, err = copilotModelIDs()
			if err != nil {
				catalogDown = true
				firstWarn = cmp.Or(firstWarn, fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err))
			}
		}
		if catalogDown {
			continue
		}
		if !slices.Contains(ids, model) {
			return "", unknownGithubModel(model, gh.Name, ids)
		}
	}
	return firstWarn, nil
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
	ids, ok := served[name]
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cat, err := CopilotModelSource(ctx, reg, name)
		cancel()
		if err != nil {
			return fmt.Sprintf("could not verify against %s serving endpoints (pin written anyway): %v", name, err), nil
		}
		for _, r := range cat.Rows {
			ids = append(ids, r.ID)
		}
		served[name] = ids
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
