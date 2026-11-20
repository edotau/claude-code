package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// ModelSpec is a typed model string split at its "<profile>:" prefix.
type ModelSpec struct {
	Row      string
	ByType   bool
	Pool     bool
	Provider string
	Model    string
}

// ErrModelSpec marks a prefix naming no usable row; callers exit 2.
var ErrModelSpec = errors.New("model spec")

// SplitModelSpec reads "<row>:<model>", "<provider type>:<model>" or the pool; any other prefix stays in the model.
func SplitModelSpec(reg *Registry, spec string) (ModelSpec, error) {
	spec = strings.TrimSpace(spec)
	name, rest, ok := strings.Cut(spec, ":")
	if !ok || name == "" || rest == "" || reg == nil {
		return ModelSpec{Model: spec}, nil
	}
	if reg.Providers[name] != nil {
		return ModelSpec{Row: name, Model: rest}, nil
	}
	if rows := reg.ProfilesOf(GroupDatabricks); name == "auto" || name == GroupDatabricks && len(rows) > 0 {
		return ModelSpec{Pool: true, Provider: GroupDatabricks, Model: rest}, nil
	}
	switch rows := reg.ProfilesOf(name); {
	case len(rows) == 1:
		return ModelSpec{Row: rows[0], ByType: true, Model: rest}, nil
	case len(rows) > 1:
		return ModelSpec{}, fmt.Errorf("%w: --model %s: %q is a provider type with several profiles (%s); name one",
			ErrModelSpec, spec, name, strings.Join(rows, ", "))
	}
	if _, _, known := models.Resolve(rest); known {
		return ModelSpec{}, fmt.Errorf("%w: --model %s: unknown profile %q", ErrModelSpec, spec, name)
	}
	// a local tag such as qwen3:30b
	return ModelSpec{Model: spec}, nil
}

// ModelCast is a typed model split at the boundary: the plain id plus the route unit its prefix named.
type ModelCast struct{ Model, Provider, Profile string }

// CastModel reads `<profile>:<model>` as `--overlay '{"provider":…,"profile":…}' --model <model>`: the one seam.
func CastModel(reg *Registry, model string) (ModelCast, error) {
	model = strings.TrimSpace(model)
	name, rest, ok := strings.Cut(model, ":")
	if !ok || name == "" || rest == "" || reg == nil {
		return ModelCast{Model: model}, nil
	}
	c, named, err := castPrefix(reg, name)
	switch {
	case err != nil:
		return ModelCast{}, fmt.Errorf("%w: model %s: %w", ErrModelSpec, model, err)
	case named && strings.Contains(rest, ":"):
		return ModelCast{}, fmt.Errorf("%w: model %s: one profile prefix at most", ErrModelSpec, model)
	case named:
		c.Model = rest
		return c, nil
	case modelWord(rest):
		return ModelCast{}, fmt.Errorf("%w: model %s: %s is not a profile (%s)", ErrModelSpec, model, name,
			strings.Join(reg.Names(), ", "))
	}
	return ModelCast{Model: model}, nil // a local tag such as qwen3:30b
}

// modelWord is auto, an alias, a family or a known model id: after an unknown prefix it means a mistyped profile.
func modelWord(s string) bool {
	_, _, known := models.Resolve(s)
	return known || s == "auto" || IsFamily(s) || slices.Contains(models.AliasNames, strings.ToLower(s))
}

// castPrefix matches name, case-insensitively, to a profile, a single-profile provider type or a pool word.
func castPrefix(reg *Registry, name string) (ModelCast, bool, error) {
	for _, n := range reg.Names() {
		if strings.EqualFold(n, name) {
			return ModelCast{Provider: reg.Providers[n].Provider, Profile: n}, true, nil
		}
	}
	lower := strings.ToLower(name)
	if lower == "auto" || lower == GroupDatabricks && len(reg.ProfilesOf(GroupDatabricks)) > 0 {
		return ModelCast{Provider: GroupDatabricks, Profile: "auto"}, true, nil
	}
	switch rows := reg.ProfilesOf(lower); {
	case len(rows) == 1:
		return ModelCast{Provider: lower, Profile: rows[0]}, true, nil
	case len(rows) > 1:
		return ModelCast{}, false, fmt.Errorf("%s is a provider with several profiles (%s); name one", name,
			strings.Join(rows, ", "))
	}
	return ModelCast{}, false, nil
}

// Merge folds c's route unit into a typed provider and profile; a different typed value is an error naming both.
func (c ModelCast) Merge(provider, profile string) (string, string, error) {
	if c.Profile == "" {
		return provider, profile, nil
	}
	if profile != "" && profile != c.Profile || provider != "" && provider != c.Provider {
		return "", "", fmt.Errorf("--model %s:%s: profile %s is outside %s; drop one",
			c.Profile, c.Model, c.Profile, strings.Trim(provider+":"+profile, ":"))
	}
	return c.Provider, c.Profile, nil
}

// Route is c's route unit as its overlay object, without the model.
func (c ModelCast) Route() string {
	return ModelCast{Provider: c.Provider, Profile: c.Profile}.Overlay()
}

// Overlay is c as the one overlay object that carries provider, profile and model; "" with no profile.
func (c ModelCast) Overlay() string {
	if c.Profile == "" {
		return ""
	}
	doc, _ := json.Marshal(struct {
		Provider string `json:"provider"`
		Profile  string `json:"profile"`
		Model    string `json:"model,omitempty"`
	}{c.Provider, c.Profile, c.Model}) // strings only: Marshal cannot fail
	return string(doc)
}

// CastShared is CastModel on the shared registry; an unloadable registry leaves model as typed for the route to judge.
func CastShared(model string) (ModelCast, error) {
	if strings.TrimSpace(model) == "" {
		return ModelCast{}, nil
	}
	reg, err := LoadShared()
	if err != nil {
		return ModelCast{Model: model}, nil
	}
	return CastModel(reg, model) // the route unit's provider is the profile's own row, never the session's
}
