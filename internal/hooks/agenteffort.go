// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool): fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet). Never overrides
// an explicit model/pin; fails OPEN on any error.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/paths"
)

// tierModel maps an effort tier to the Agent tool's model alias. All three tiers are pinned: an absent
// TierHigh meant a verbose prompt inherited the PARENT session model, so any long "audit"/"implement"
// ask silently ran on Opus. An explicit model: on the dispatch still wins over every row here.
var tierModel = map[string]string{
	TierLow:    "haiku",
	TierMedium: "sonnet",
	TierHigh:   "sonnet",
}

// AgentEffort fills an unset spawn model with the classifier's cost-down tier; emits nothing (and
// proceeds) in every other case.
func AgentEffort(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	if !spawnTools[in.ToolName] {
		return ExitProceed
	}
	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model.
	if m := strings.TrimSpace(in.nested("tool_input", "model")); m != "" && !strings.EqualFold(m, "inherit") {
		return ExitProceed
	}
	// The roster's own pin outranks every decision: a model-pinned agent already carries its tier.
	if agentPinsModel(in.nested("tool_input", "subagent_type")) {
		return ExitProceed
	}
	task := in.nested("tool_input", "prompt")
	if task == "" {
		task = in.nested("tool_input", "description")
	}
	if task == "" {
		return ExitProceed
	}
	tier := EffortTier(task)
	model := tierModel[tier]
	if model == "" {
		return ExitProceed // a tier with no row: never emit an empty pin
	}
	note := fmt.Sprintf("agent-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
		" (set model explicitly to override).", tier, model)

	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (isolation, run_in_background, subagent_type, …) would silently alter the dispatch.
	updated := agentToolInput(in)
	if updated == nil {
		return ExitProceed
	}
	updated["model"] = model

	inner := map[string]any{"hookEventName": "PreToolUse", "additionalContext": note, "updatedInput": updated}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": inner})
	if err != nil {
		return ExitProceed
	}
	fmt.Fprintln(stdout, string(out))
	return ExitProceed
}

// agentToolInput clones tool_input so the caller can rewrite one key without mutating the parsed doc.
func agentToolInput(in *Input) map[string]any {
	var doc struct {
		ToolInput map[string]any `json:"tool_input"`
	}
	if json.Unmarshal(in.Raw, &doc) != nil || doc.ToolInput == nil {
		return nil
	}
	clone := make(map[string]any, len(doc.ToolInput)+1)
	for k, v := range doc.ToolInput {
		clone[k] = v
	}
	return clone
}

// agentPinsModel reports whether the named subagent's frontmatter pins a concrete model (not
// `inherit`). A name with no roster file is a built-in (Explore/Plan/general-purpose) — not pinned,
// the classifier applies; only a path-shaped name is refused outright.
func agentPinsModel(name string) bool {
	if name == "" {
		return false // no subagent_type = a fresh general-purpose agent, nothing to defer to
	}
	if filepath.Base(name) != name {
		return true // path-shaped input is not a roster name; refuse to touch the dispatch
	}
	raw, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "agents", name+".md"))
	if err != nil {
		return false // not a harness agent (built-in like Explore/Plan) — classifier may apply
	}
	fm, _ := crossgen.ParseFrontmatter(string(raw))
	model, _ := fm["model"].(string)
	return model != "" && model != "inherit"
}
