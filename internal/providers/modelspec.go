package providers

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ModelCast is a checked model input: the plain id (Provider/Profile only fill Overlay's suggestion).
type ModelCast struct{ Model, Provider, Profile string }

// ModelProfileError refuses a model name carrying a profile, naming the overlay form to use instead.
type ModelProfileError struct{ Model, Overlay, Plain string }

func (e *ModelProfileError) Error() string {
	return fmt.Sprintf("model %s: a model name does not carry a profile: use --overlay '%s'", e.Model, e.Overlay)
}

func (e *ModelProfileError) Unwrap() error { return ErrModelSpec }

// CastModel is the one model-input check: a plain id passes, a `<profile>:<model>` value is refused (ModelProfileError).
func CastModel(reg *Registry, model string) (ModelCast, error) {
	ms, err := SplitModelSpec(reg, model)
	c := ModelCast{Model: ms.Model}
	switch {
	case err != nil:
		return ModelCast{}, err
	case ms.Pool:
		c.Provider, c.Profile = GroupDatabricks, "auto"
	case ms.Row != "":
		c.Provider, c.Profile = reg.Providers[ms.Row].Provider, ms.Row
	default:
		return c, nil
	}
	return ModelCast{}, &ModelProfileError{Model: strings.TrimSpace(model), Overlay: c.Overlay(), Plain: c.Model}
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
	return CastModel(reg, model)
}
