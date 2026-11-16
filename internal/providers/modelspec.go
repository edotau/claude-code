package providers

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// ModelSpec is a typed model string split at its "<profile>:" prefix.
type ModelSpec struct {
	Row    string // registry row the prefix names; "" when none was typed
	ByType bool   // the prefix was a provider type with exactly one row
	Model  string // the rest as typed, [1m] kept
}

// ErrModelSpec marks a prefix naming no usable row; callers exit 2.
var ErrModelSpec = errors.New("model spec")

// SplitModelSpec reads "<row>:<model>" or "<provider type>:<model>" (one row); any other prefix stays in the model.
func SplitModelSpec(reg *Registry, spec string) (ModelSpec, error) {
	spec = strings.TrimSpace(spec)
	name, rest, ok := strings.Cut(spec, ":")
	if !ok || name == "" || rest == "" || reg == nil {
		return ModelSpec{Model: spec}, nil
	}
	if reg.Providers[name] != nil {
		return ModelSpec{Row: name, Model: rest}, nil
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
	return ModelSpec{Model: spec}, nil // a local tag such as qwen3:30b
}
