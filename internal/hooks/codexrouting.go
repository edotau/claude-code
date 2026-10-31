package hooks

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
)

// nativeCodexAgentRoute fills a fresh, unpinned child's model; the builder renders the updated input as an allow.
func nativeCodexAgentRoute(in *Input) Result {
	if in.Agent() != AgentCodex || !codexSpawnTool(in.ToolName) || !codexFreshSpawn(in) {
		return Result{}
	}
	updated := in.cloneToolInput()
	task := strings.TrimSpace(in.nested("tool_input", "message"))
	if updated == nil || task == "" || codexSpawnPinned(in) {
		return Result{}
	}
	cell, signal, ok := codexSpawnCell(in, task)
	if !ok {
		return Result{}
	}
	updated["model"] = cell.Model
	if cell.Effort != "" {
		updated["reasoning_effort"] = cell.Effort
	}
	res := codexRecorded(in, codexRouteRecord{Event: "decision", ToolUseID: in.nested("tool_use_id"),
		RequestedModel: cell.Model, Effort: cell.Effort, Signal: signal})
	res.UpdatedInput = updated
	return res
}

func codexSpawnTool(name string) bool { return strings.HasSuffix(name, "spawn_agent") }

// Only an explicit fresh-history request permits Codex model/effort overrides.
func codexFreshSpawn(in *Input) bool {
	args := in.ToolInput()
	if fork, ok := args["fork_turns"]; ok {
		return fork == "none"
	}
	fork, ok := args["fork_context"].(bool)
	return ok && !fork
}

func codexSpawnPinned(in *Input) bool {
	for _, key := range []string{"model", "reasoning_effort"} {
		if value, present := in.ToolInput()[key]; present && value != nil && value != "" {
			return true
		}
	}
	role := in.nested("tool_input", "agent_type")
	if role != "" && role != "default" {
		return true
	}
	for _, key := range []string{providers.PinCodexModel, providers.PinCodexEffort} {
		if providers.Pin(key) != "" {
			return true
		}
	}
	return false
}

func codexSpawnCell(in *Input, task string) (router.Cell, string, bool) {
	reg, err := providers.Load()
	if err != nil {
		return router.Cell{}, "", false
	}
	p := reg.Providers[os.Getenv(settings.CodexProfileEnv)]
	if p == nil || p.Preset != providers.PresetDatabricks || !providers.ServesResponses(p, in.nested("model")) {
		return router.Cell{}, "", false
	}
	table, err := router.LoadFrom(reg, "")
	if err != nil {
		return router.Cell{}, "", false
	}
	cell := table.Rows["codex"][router.RuleClassifier{}.Tier(task)]
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
