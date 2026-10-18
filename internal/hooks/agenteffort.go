// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool): fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet), and prefixes a
// vendor-routed agent's (copilot/opencode/codex/gemini) description with the model its leg resolves
// to, so the UI shows e.g. "copilot(gpt-6.1-sol · repair clean tests)". Never overrides an explicit
// model/pin; fails OPEN on any error.
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
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// tierModel maps an effort tier to the Agent tool's model alias. All three tiers are pinned: an absent
// TierHigh meant a verbose prompt inherited the PARENT session model, so any long "audit"/"implement"
// ask silently ran on Opus. An explicit model: on the dispatch still wins over every row here.
var tierModel = map[string]string{
	TierLow:    "haiku",
	TierMedium: "sonnet",
	TierHigh:   "sonnet",
}

// AgentEffort prefixes a vendor-routed agent's description with its resolved model (applies ahead
// of, and independent from, the model-pin decision below), then fills an unset spawn model with the
// classifier's cost-down tier; emits nothing (and proceeds) when neither change applies.
func AgentEffort(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	if !spawnTools[in.ToolName] {
		return ExitProceed
	}
	subagent := in.nested("tool_input", "subagent_type")

	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (isolation, run_in_background, subagent_type, …) would silently alter the dispatch.
	updated := agentToolInput(in)
	if updated == nil {
		return ExitProceed
	}

	var notes []string
	if word, ok := vendorModelWord(subagent); ok {
		if desc, ok := updated["description"].(string); ok {
			if prefixed := prefixDescription(desc, word); prefixed != desc {
				updated["description"] = prefixed
				notes = append(notes, fmt.Sprintf("agent-effort: prefixed description with %s's routed model %q.", subagent, word))
			}
		}
	}

	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model.
	hasExplicitModel := func() bool {
		m := strings.TrimSpace(in.nested("tool_input", "model"))
		return m != "" && !strings.EqualFold(m, "inherit")
	}()
	// The roster's own pin outranks every decision: a model-pinned agent already carries its tier.
	if !hasExplicitModel && !agentPinsModel(subagent) {
		task := in.nested("tool_input", "prompt")
		if task == "" {
			task = in.nested("tool_input", "description")
		}
		if task != "" {
			tier := EffortTier(task)
			if model := tierModel[tier]; model != "" { // a tier with no row: never emit an empty pin
				updated["model"] = model
				notes = append(notes, fmt.Sprintf("agent-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
					" (set model explicitly to override).", tier, model))
			}
		}
	}

	if len(notes) == 0 {
		return ExitProceed
	}
	inner := map[string]any{"hookEventName": "PreToolUse", "additionalContext": strings.Join(notes, " "), "updatedInput": updated}
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": inner})
	if err != nil {
		return ExitProceed
	}
	fmt.Fprintln(stdout, string(out))
	return ExitProceed
}

// vendorPrefixAgents are the four subagent_type names whose spawn routes through a third-party CLI.
var vendorPrefixAgents = map[string]bool{"copilot": true, "opencode": true, "codex": true, "gemini": true}

// vendorModelWord resolves the model name (or leg/reason word on a dead route) a vendor-routed
// agent's dispatch leg serves today; ok is false for every non-vendor name and on any registry
// load error (fail open — never block the spawn over a routing lookup).
func vendorModelWord(subagentType string) (word string, ok bool) {
	if !vendorPrefixAgents[subagentType] {
		return "", false
	}
	reg, err := providers.Load()
	if err != nil {
		return "", false
	}
	switch subagentType {
	case "copilot", "opencode":
		ri := vendors.DescribeRoute(reg, subagentType)
		if ri.Model != "" {
			return ri.Model, true
		}
		return "no-route", true
	case "codex", "gemini":
		sel, err := providers.Select(reg, "")
		if err != nil {
			return "no-route", true
		}
		target, _, err := vendors.ResolveTarget(reg, subagentType, sel, false)
		if err != nil {
			return "no-route", true
		}
		if target.Model != "" {
			return target.Model, true
		}
		return "no-route", true
	default:
		return "", false
	}
}

// descriptionCap is the longest prefixed description this hook will emit; Claude Code's own limit on
// the Agent tool's description field is undocumented, so this stays conservative.
const descriptionCap = 80

// anyWordSep is "<word> · " at the start of a description — the prefix this hook writes, or one
// already written by a prior pass; idempotent either way.
var anyWordSep = " · "

// prefixDescription returns "<word> · <desc>", truncating desc so the result fits descriptionCap; a
// desc that already starts with "<anything> · " is left untouched (idempotent), as is an empty word.
func prefixDescription(desc, word string) string {
	if desc == "" || word == "" {
		return desc
	}
	if i := strings.Index(desc, anyWordSep); i >= 0 && !strings.ContainsAny(desc[:i], " \t\n") {
		return desc // already prefixed by this hook (or hand-authored the same way) — leave it
	}
	prefix := word + anyWordSep
	budget := descriptionCap - len([]rune(prefix))
	body := []rune(desc)
	if budget < 1 {
		return prefix // word alone already exceeds the cap; nothing left for the body
	}
	if len(body) <= budget {
		return prefix + desc
	}
	if budget == 1 {
		return prefix + "…"
	}
	return prefix + string(body[:budget-1]) + "…"
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
