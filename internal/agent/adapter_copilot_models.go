package agent

import (
	"context"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
)

// CopilotCatalog is where `copilot models` lists from: a Databricks row's endpoints, or GitHub's own catalog.
type CopilotCatalog struct {
	Source     string
	Profile    string
	Databricks bool
	Rows       []copilotreg.Endpoint
}

// CopilotModelSource resolves profile (default: the session profile) and lists its chat endpoints if Databricks.
func CopilotModelSource(ctx context.Context, reg *providers.Registry, profile string) (CopilotCatalog, error) {
	name := profile
	if name == "" {
		name = providers.SessionProfile()
	}
	p, ok := reg.Providers[name]
	if !ok || p == nil || p.Preset != providers.PresetDatabricks {
		return CopilotCatalog{Source: "GitHub Copilot catalog"}, nil
	}
	rows, err := copilotreg.Discover(ctx, reg, p)
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true, Rows: rows}, nil
}
