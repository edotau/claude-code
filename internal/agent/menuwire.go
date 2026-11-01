package agent

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// OwnedID namespaces the provider entry so it never overwrites a user's own block of the same name.
func OwnedID(p *providers.Provider) (string, error) {
	if !idRE.MatchString(p.Name) {
		return "", fmt.Errorf("provider name %q must match %s to be passed to an agent config", p.Name, idRE)
	}
	return "harness-" + p.Name, nil
}

// CatalogWarn receives catalog warnings (codex and opencode menus).
var CatalogWarn io.Writer = os.Stderr

// SetCatalogWarn redirects catalog warnings and returns the previous writer; tests use it to keep stderr quiet.
func SetCatalogWarn(w io.Writer) io.Writer { old := CatalogWarn; CatalogWarn = w; return old }

// MenuWire spells a bare menu id the way p serves it; an id outside the catalog still takes the row's prefix once.
func MenuWire(p *providers.Provider, id string) string {
	if w := p.WireModel(id); w != id {
		return w
	}
	rule := p.SpellingRule()
	if _, _, known := models.Resolve(id); known || rule == "" ||
		strings.HasPrefix(id, models.DatabricksPrefix) || strings.HasPrefix(id, "system.ai.") {
		return id
	}
	return models.Wire(rule, id)
}
