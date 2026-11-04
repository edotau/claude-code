package hooks

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
)

// nativeCodexAgentRoute fills a fresh, unpinned child's model; the builder renders the updated input as an allow.
func nativeCodexAgentRoute(in *core.Input) core.Result {
	if in.Agent() != core.AgentCodex || !codexSpawnTool(in.ToolName) || !codexFreshSpawn(in) {
		return core.Result{}
	}
	updated := in.CloneToolInput()
	task := strings.TrimSpace(in.Nested("tool_input", "message"))
	if updated == nil || task == "" || codexSpawnPinned(in) {
		return core.Result{}
	}
	cell, signal, ok := codexSpawnCell(in, task)
	if !ok {
		return core.Result{}
	}
	updated["model"] = cell.Model
	if cell.Effort != "" {
		updated["reasoning_effort"] = cell.Effort
	}
	res := codexRecorded(in, codexRouteRecord{Event: "decision", ToolUseID: in.Nested("tool_use_id"),
		RequestedModel: cell.Model, Effort: cell.Effort, Signal: signal})
	res.UpdatedInput = updated
	return res
}

func codexSpawnTool(name string) bool { return strings.HasSuffix(name, "spawn_agent") }

// Only an explicit fresh-history request permits Codex model/effort overrides.
func codexFreshSpawn(in *core.Input) bool {
	args := in.ToolInput()
	if fork, ok := args["fork_turns"]; ok {
		return fork == "none"
	}
	fork, ok := args["fork_context"].(bool)
	return ok && !fork
}

func codexSpawnPinned(in *core.Input) bool {
	for _, key := range []string{"model", "reasoning_effort"} {
		if value, present := in.ToolInput()[key]; present && value != nil && value != "" {
			return true
		}
	}
	role := in.Nested("tool_input", "agent_type")
	if role != "" && role != "default" {
		return true
	}
	rec := providers.AgentRecord("codex")
	return rec.Model != "" || rec.Effort != ""
}

func codexSpawnCell(in *core.Input, task string) (router.Cell, string, bool) {
	reg, err := providers.Load()
	if err != nil {
		return router.Cell{}, "", false
	}
	p := reg.Providers[os.Getenv(settings.CodexProfileEnv)]
	if p == nil || p.Preset != providers.PresetDatabricks || !providers.ServesResponses(p, in.Nested("model")) {
		return router.Cell{}, "", false
	}
	table, err := router.LoadFrom(reg, "")
	if err != nil {
		return router.Cell{}, "", false
	}
	cell := table.Rows["codex"][router.RuleClassifier{}.Alias(task)]
	cell.Model = p.WireModel(cell.Model)
	if !providers.ServesResponses(p, cell.Model) || !codexListedModel(cell.Model) {
		return router.Cell{}, "", false
	}
	_, signal := router.EffortTierSignal(task)
	return cell, signal, true
}

func codexListedModel(model string) bool {
	body, err := os.ReadFile(os.Getenv(settings.CodexCatalogEnv))
	if err != nil {
		return false
	}
	var catalog struct {
		Models []struct{ Slug, Visibility string }
	}
	if json.Unmarshal(body, &catalog) != nil {
		return false
	}
	for _, row := range catalog.Models {
		if row.Slug == model && row.Visibility == "list" {
			return true
		}
	}
	return false
}
