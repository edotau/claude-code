package vendoragent

import (
	"context"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// CopilotModel is one serving endpoint copilot can reach; Tested marks endpoints on the curated menu.
type CopilotModel struct {
	providers.ModelInfo
	Tested bool
}

// CopilotCatalog is where `copilot models` lists from: a Databricks row's endpoints, or GitHub's own catalog.
type CopilotCatalog struct {
	Source     string
	Profile    string
	Databricks bool
	Rows       []CopilotModel
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
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true,
		Rows: ServingChatRows(infos, providers.OpencodeMenu())}, nil
}

// ServingChatRows keeps READY llm/v1/chat endpoints in order, marking those on menu (with or without "databricks-").
func ServingChatRows(infos []providers.ModelInfo, menu []providers.OpencodeModel) []CopilotModel {
	onMenu := make(map[string]bool, len(menu))
	for _, m := range menu {
		onMenu[m.ID] = true
	}
	var out []CopilotModel
	for _, info := range infos {
		if info.Task != "llm/v1/chat" || info.Ready != "READY" {
			continue
		}
		tested := onMenu[info.ID] || onMenu[strings.TrimPrefix(info.ID, models.DatabricksPrefix)]
		out = append(out, CopilotModel{ModelInfo: info, Tested: tested})
	}
	return out
}
