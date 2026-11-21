package providers

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// ErrModelSpec marks a model string that carries a profile prefix; callers exit 2.
var ErrModelSpec = errors.New("model spec")

// PrefixError refuses `<x>:<model>`: a profile travels as the route, never inside the model string.
type PrefixError struct {
	Spec, Provider, Profile, Model string
	Detail                         string // why the prefix names no single row, when it does not
}

func (e *PrefixError) Unwrap() error { return ErrModelSpec }

func (e *PrefixError) Error() string {
	msg := fmt.Sprintf("model %s: a profile is not part of a model", e.Spec)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg + fmt.Sprintf("; use --overlay '%s' --model %s", e.Overlay(), e.Model)
}

// Overlay is the route object the refusal names; placeholders stand in for a part the prefix did not settle.
func (e *PrefixError) Overlay() string {
	return fmt.Sprintf(`{"provider":%q,"profile":%q}`, cmp.Or(e.Provider, "<provider>"), cmp.Or(e.Profile, "<profile>"))
}

// CheckModel refuses a model whose prefix names a row, a provider type or auto, or an unknown prefix on a model word.
func CheckModel(reg *Registry, model string) error {
	model = strings.TrimSpace(model)
	name, rest, ok := strings.Cut(model, ":")
	if !ok || name == "" || rest == "" {
		return nil
	}
	if reg == nil {
		reg = &Registry{}
	}
	e, named := prefixRoute(reg, name)
	if !named && !modelWord(rest) {
		return nil // a local tag such as qwen3:30b
	}
	e.Spec, e.Model = model, rest
	if !named {
		e.Detail = fmt.Sprintf("%s is not a profile: %s", name, strings.Join(reg.Names(), ", "))
	}
	return e
}

// CheckModelShared is CheckModel on the shared registry; an unloadable one still refuses auto and model-word prefixes.
func CheckModelShared(model string) error {
	reg, _ := LoadShared()
	return CheckModel(reg, model)
}

// modelWord is auto, an alias, a family or a known model id: after an unknown prefix it means a mistyped profile.
func modelWord(s string) bool {
	_, _, known := models.Resolve(s)
	return known || strings.EqualFold(s, "auto") || IsFamily(s) || slices.Contains(models.AliasNames, strings.ToLower(s))
}

// prefixRoute matches name, case-insensitively, to a profile, a provider type or the pool word.
func prefixRoute(reg *Registry, name string) (*PrefixError, bool) {
	for _, n := range reg.Names() {
		if strings.EqualFold(n, name) {
			return &PrefixError{Provider: reg.Providers[n].Provider, Profile: n}, true
		}
	}
	lower := strings.ToLower(name)
	if lower == "auto" || lower == GroupDatabricks && len(reg.ProfilesOf(GroupDatabricks)) > 0 {
		return &PrefixError{Provider: GroupDatabricks, Profile: "auto"}, true
	}
	switch rows := reg.ProfilesOf(lower); {
	case len(rows) == 1:
		return &PrefixError{Provider: lower, Profile: rows[0]}, true
	case len(rows) > 1:
		return &PrefixError{Provider: lower, Detail: name + " has several profiles: " + strings.Join(rows, ", ")}, true
	}
	return &PrefixError{}, false
}
