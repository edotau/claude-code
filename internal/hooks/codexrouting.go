package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/routetable"
)

// CodexAgentRoute fills a fresh, unpinned child's model without changing its inherited provider.
func CodexAgentRoute(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	if in.Vendor() != VendorCodex || !codexSpawnTool(in.ToolName) || !codexFreshSpawn(in) {
		return ExitProceed
	}
	updated := in.cloneToolInput()
	task := strings.TrimSpace(in.nested("tool_input", "message"))
	if updated == nil || task == "" || codexSpawnPinned(in) {
		return ExitProceed
	}
	cell, signal, ok := codexSpawnCell(in, task)
	if !ok {
		return ExitProceed
	}
	updated["model"] = cell.Model
	if cell.Effort != "" {
		updated["reasoning_effort"] = cell.Effort
	}
	output := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": updated,
	}}
	body, err := json.Marshal(output)
	if err != nil {
		return ExitProceed
	}
	recordCodexRoute(in, codexRouteRecord{Event: "decision", ToolUseID: in.nested("tool_use_id"),
		RequestedModel: cell.Model, Effort: cell.Effort, Signal: signal}, stderr)
	fmt.Fprintln(stdout, string(body))
	return ExitProceed
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

func codexSpawnCell(in *Input, task string) (routetable.Cell, string, bool) {
	reg, err := providers.Load()
	if err != nil {
		return routetable.Cell{}, "", false
	}
	p := reg.Providers[os.Getenv(hookspec.CodexProfileEnv)]
	if p == nil || p.Preset != providers.PresetDatabricks || !providers.ServesResponses(p, in.nested("model")) {
		return routetable.Cell{}, "", false
	}
	table, err := routetable.LoadFrom(reg, "")
	if err != nil {
		return routetable.Cell{}, "", false
	}
	cell := table.Rows["codex"][RuleClassifier{}.Tier(task)]
	cell.Model = p.WireModel(cell.Model)
	if !providers.ServesResponses(p, cell.Model) || !codexListedModel(cell.Model) {
		return routetable.Cell{}, "", false
	}
	_, signal := EffortTierSignal(task)
	return cell, signal, true
}

func codexListedModel(model string) bool {
	body, err := os.ReadFile(os.Getenv(hookspec.CodexCatalogEnv))
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
