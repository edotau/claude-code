package copilotreg

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Named selection failures; callers match with errors.Is.
var (
	ErrUnknownModel      = errors.New("model is not in the registry")
	ErrAmbiguousModel    = errors.New("model selection is ambiguous")
	ErrSavedModelGone    = errors.New("saved model left the registry")
	ErrNoModels          = errors.New("registry has no models")
	ErrRepeatedSelection = errors.New("model selected more than once")
)

// Choice is every place a model can come from, highest priority first: Requested (each --model given),
// Saved (a resumed session's provider/id), Pin (the vault pin value); none set picks the default.
type Choice struct {
	Requested []string
	Saved     string
	Pin       string
}

// Select maps a harness alias | canonical id | wire id | provider/id to the registry's provider/id.
func (r Registry) Select(c Choice) (string, error) {
	if len(r.Models) == 0 {
		return "", ErrNoModels
	}
	switch {
	case len(c.Requested) > 1:
		return "", fmt.Errorf("%w: %s", ErrRepeatedSelection, strings.Join(c.Requested, ", "))
	case len(c.Requested) == 1:
		return r.find(c.Requested[0], ErrUnknownModel)
	case c.Saved != "":
		return r.find(c.Saved, ErrSavedModelGone)
	case c.Pin != "":
		return r.find(c.Pin, ErrSavedModelGone)
	}
	return r.defaultRef(), nil
}

// defaultRef is the sonnet alias's model when the profile serves it, else the first model.
func (r Registry) defaultRef() string {
	if canonical, ok := models.Alias(models.Sonnet); ok {
		for _, m := range r.Models {
			if m.ID == canonical {
				return m.Ref()
			}
		}
	}
	return r.Models[0].Ref()
}

// find matches s by provider/id, then id, then wire id, then its canonical resolution; a miss wraps miss.
func (r Registry) find(s string, miss error) (string, error) {
	s = strings.TrimSpace(s)
	canonical, _, _ := models.Resolve(s)
	for _, match := range []func(Model) bool{
		func(m Model) bool { return m.Ref() == s },
		func(m Model) bool { return m.ID == s },
		func(m Model) bool { return m.WireModel == s },
		func(m Model) bool { return canonical != "" && m.ID == canonical },
	} {
		var hits []string
		for _, m := range r.Models {
			if match(m) {
				hits = append(hits, m.Ref())
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			return "", fmt.Errorf("%w: %q matches %s", ErrAmbiguousModel, s, strings.Join(hits, ", "))
		}
	}
	return "", fmt.Errorf("%w: %q (profile %s)", miss, s, r.Profile)
}
