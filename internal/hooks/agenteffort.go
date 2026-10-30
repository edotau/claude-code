// `agent-effort` — PreToolUse on a subagent spawn (Claude Code's Agent/Task tool) or a Workflow: fills an UNSET
// dispatch model from the local prompt classifier (low->haiku, medium/high->sonnet). Never overrides
// an explicit model/pin; fails OPEN on any error.
package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/edotau/claude-code/internal/frontmatter"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
)

// RuleClassifier is route.Classifier over the prompt rules: low → haiku, medium/high → sonnet.
type RuleClassifier struct{}

// Tier maps task through EffortTierSignal and models.TierModel.
func (RuleClassifier) Tier(task string) string {
	t, _ := EffortTierSignal(task)
	return models.TierModel(t)
}

// labelAgents are the agents a dispatch label may name; labelTypes are the subagent types that carry one.
var (
	labelAgents = map[string]bool{"claude": true, "codex": true, "copilot": true, "gemini": true, "opencode": true}
	labelTypes  = map[string]bool{"claude": true, "code-worker": true, "coworker": true,
		"codex": true, "copilot": true, "gemini": true, "opencode": true}
	agentLineRe = regexp.MustCompile(`(?m)^[ \t]*Agent:[ \t]*(.+)$`)
	agentNameRe = regexp.MustCompile(`[a-z]+`)
)

// agentEffortResult is the pin step as a Result: a spawn gets one UpdatedInput (model pin, recall block, quota
// pressure, label); a Workflow gets the former workflow-lint rewrite or a Block.
func agentEffortResult(in *Input) Result {
	if in.ToolName == "Workflow" {
		return workflowResult(in)
	}
	if !spawnTools[in.ToolName] {
		return Result{}
	}
	// updatedInput REPLACES the whole tool_input record, so every original key must be echoed back;
	// dropping one (isolation, run_in_background, subagent_type, …) would silently alter the dispatch.
	updated := in.cloneToolInput()
	if updated == nil {
		return Result{}
	}

	promptField := "prompt"
	task := in.nested("tool_input", "prompt")
	if task == "" {
		promptField = "description"
		task = in.nested("tool_input", "description")
	}

	recallBlock, _ := agentRecall(in.SessionID, in.CWD, task)
	changed := recallBlock != ""
	if changed {
		updated[promptField] = task + recallBlock
	}

	note, systemMessage := pinModel(in, updated, task)
	if !explicitModel(in) {
		base, _ := updated["model"].(string)
		if base == "" {
			base = rosterModel(in.nested("tool_input", "subagent_type"))
		}
		if pn, ps := applyPressure(in, updated, promptField, base); pn != "" {
			note, systemMessage = strings.TrimSpace(note+" "+pn), strings.TrimSpace(systemMessage+"; "+ps)
			systemMessage = strings.TrimPrefix(systemMessage, "; ")
		}
	}
	if applyLabel(updated, task) {
		changed = true
	}
	if note != "" {
		changed = true
	}
	if !changed {
		return Result{}
	}
	return Result{UpdatedInput: updated, Context: note, SystemMessage: systemMessage}
}

// applyLabel prefixes the description with the agents the `Agent:` line sends work to, minus the leaf's own type.
func applyLabel(updated map[string]any, task string) bool {
	st, _ := updated["subagent_type"].(string)
	if !labelTypes[st] {
		return false
	}
	desc, _ := updated["description"].(string)
	label := agentLabel(st, task)
	var out string
	switch {
	case label == "":
		out = lowerFirst(desc)
	case strings.HasPrefix(desc, label+":"):
		out = label + ": " + lowerFirst(strings.TrimPrefix(desc[len(label)+1:], " "))
	case desc == label:
		return false
	case strings.TrimSpace(desc) == "":
		out = label
	default:
		out = label + ": " + lowerFirst(desc)
	}
	if out == desc {
		return false
	}
	updated["description"] = out
	return true
}

// modeCompareRe matches a `Mode: compare` line in a brief.
var modeCompareRe = regexp.MustCompile(`(?im)^\s*Mode:\s*compare\b`)

// agentLabel is the "a → b" (relay) or "a, b" (`Mode: compare`) label the `Agent:` line earns, or "" when there is none.
func agentLabel(st, task string) string {
	m := agentLineRe.FindStringSubmatch(task)
	if m == nil {
		return ""
	}
	line := strings.ToLower(m[1])
	if strings.Contains(line, "in-context") {
		return ""
	}
	var names []string
	seen := map[string]bool{}
	for _, w := range agentNameRe.FindAllString(line, -1) {
		if labelAgents[w] && w != st && !seen[w] {
			seen[w] = true
			names = append(names, w)
		}
	}
	if modeCompareRe.MatchString(task) { // a compare fans out in parallel: names, not a chain
		return strings.Join(names, ", ")
	}
	return strings.Join(names, " → ")
}

// lowerFirst lowercases desc's first letter unless its first word looks like an acronym or identifier.
func lowerFirst(desc string) string {
	word := strings.TrimRight(strings.Fields(desc + " x")[0], ".,:;!?")
	if strings.ContainsAny(word, "._/`0123456789") {
		return desc
	}
	r := []rune(desc)
	i := 0
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	if i >= len(r) || !unicode.IsUpper(r[i]) || (i+1 < len(r) && unicode.IsUpper(r[i+1])) {
		return desc
	}
	r[i] = unicode.ToLower(r[i])
	return string(r)
}

// pinModel fills updated["model"] from the classifier when the dispatch left it unset,
// and returns the model note and visible reason ("" when nothing was pinned).
func pinModel(in *Input, updated map[string]any, task string) (note, systemMessage string) {
	// An explicit model is a deliberate choice by whoever dispatched — never second-guess it.
	// `inherit` is not one of those: it is the default spelling for "unset", and honoring it ran
	// every unpinned spawn on the parent session's model. The roster's own pin outranks every
	// decision too: a model-pinned agent already carries its tier.
	if explicitModel(in) || task == "" || agentPinsModel(in.nested("tool_input", "subagent_type")) {
		return "", ""
	}
	tier, signal := EffortTierSignal(task)
	model := models.TierModel(tier)
	if model == "" { // a tier with no row: never emit an empty pin
		return "", ""
	}
	updated["model"] = model
	return fmt.Sprintf("agent-effort: dispatch model unset and the task classifies %s — pinned model=%s"+
			" (set model explicitly to override).", tier, model),
		fmt.Sprintf("agent-effort: %s → %s (%s)", tier, model, signal)
}

// explicitModel is a dispatch-set model other than `inherit` (the default spelling for unset).
func explicitModel(in *Input) bool {
	m := strings.TrimSpace(in.nested("tool_input", "model"))
	return m != "" && !strings.EqualFold(m, "inherit")
}

// rosterModel is the named harness agent's frontmatter model ("" for a built-in or a path-shaped name).
func rosterModel(name string) string {
	if name == "" || filepath.Base(name) != name {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "agents", name+".md"))
	if err != nil {
		return ""
	}
	fm, _ := frontmatter.Parse(string(raw))
	model, _ := fm["model"].(string)
	return model
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
