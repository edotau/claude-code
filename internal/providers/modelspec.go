package providers

import (
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
