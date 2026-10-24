// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool): fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet). Never overrides
// an explicit model/pin; fails OPEN on any error.
package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/frontmatter"
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

// AgentEffort fills an unset spawn model with the classifier's cost-down tier, AND appends a memory-recall
// pointer block to the spawn prompt (a subagent gets no UserPromptSubmit, so this is its only recall
// surface) — both rewrites share ONE updatedInput, since two hooks each rewriting it on the same event race.
// Emits nothing when neither applies.
func AgentEffort(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	if !spawnTools[in.ToolName] {
		return ExitProceed
	}
	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (isolation, run_in_background, subagent_type, …) would silently alter the dispatch.
	updated := in.cloneToolInput()
	if updated == nil {
		return ExitProceed
	}

	promptField := "prompt"
	task := in.nested("tool_input", "prompt")
	if task == "" {
		promptField = "description"
		task = in.nested("tool_input", "description")
	}

	recallBlock, recallIDs := agentRecall(in.SessionID, in.CWD, task)
	promptChanged := recallBlock != ""
	if promptChanged {
		updated[promptField] = task + recallBlock
	}

	note, systemMessage := pinModel(in, updated, task, recallIDs)
	modelChanged := note != ""

	if !modelChanged && !promptChanged {
		return ExitProceed // nothing rewritten
	}

	inner := map[string]any{"hookEventName": "PreToolUse", "updatedInput": updated}
	if note != "" {
		inner["additionalContext"] = note
	}
	payload := map[string]any{"hookSpecificOutput": inner}
	if systemMessage != "" {
		payload["systemMessage"] = systemMessage
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return ExitProceed
	}
	fmt.Fprintln(stdout, string(out))
	return ExitProceed
}

// pinModel fills updated["model"] from the classifier when the dispatch left it unset, logs the decision,
// and returns the model note and visible reason ("" when nothing was pinned).
func pinModel(in *Input, updated map[string]any, task string, recallIDs []string) (note, systemMessage string) {
	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model. The roster's own pin outranks every
	// decision too: a model-pinned agent already carries its tier.
	explicit := strings.TrimSpace(in.nested("tool_input", "model")) != "" &&
		!strings.EqualFold(in.nested("tool_input", "model"), "inherit")
	if explicit || task == "" || agentPinsModel(in.nested("tool_input", "subagent_type")) {
		return "", ""
	}
	tier, signal := EffortTierSignal(task)
	model := tierModel[tier]
	if model == "" { // a tier with no row: never emit an empty pin
		return "", ""
	}
	updated["model"] = model
	decision := map[string]any{
		"decision_id":   randomHex(8),
		"session_id":    in.SessionID,
		"tool_use_id":   in.nested("tool_use_id"),
		"subagent_type": in.nested("tool_input", "subagent_type"),
		"tier":          tier,
		"signal":        signal,
		"model":         model,
		"at":            time.Now().Unix(),
	}
	if len(recallIDs) > 0 {
		decision["recall_ids"] = recallIDs
	}
	appendDecision(decision)
	return fmt.Sprintf("agent-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
			" (set model explicitly to override).", tier, model),
		fmt.Sprintf("agent-effort: %s → %s (%s)", tier, model, signal)
}

// randomHex is a decision_id: collision-safe enough for a best-effort reconciliation log; a read
// failure on crypto/rand (never on a real OS) falls back to a time-based id rather than an empty one.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// agentPinsModel reports whether the named subagent's frontmatter sets any model, `inherit` included (it
// asks for the session model). No roster file = a built-in, the classifier applies; a path-shaped name is refused.
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
	fm, _ := frontmatter.Parse(string(raw))
	model, _ := fm["model"].(string)
	return model != ""
}
