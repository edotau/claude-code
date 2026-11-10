// `workflow-steps` — PreToolUse on Workflow: checks a saved script's args.steps (and profile/provider) the way the
// scripts' stepOf would, then dry-runs the route of every external stage, so a bad call blocks before any agent starts.
package quality

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/router"
)

// The values stepOf accepts; the scripts keep only an unknown-stage/field backstop.
var (
	stepClaudeModels = []string{"haiku", "sonnet", "opus", "fable"}
	stepAgents       = agentid.All()
	stepEfforts      = agentid.EffortNames()
	stepFields       = []string{"agent", "model", "effort", "profile", "provider"}
	stepNameRe       = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._-]+$`) })
	// stepStageRe finds a script's own stages: stepOf(a.steps|OWN, '<stage>', …); a child's stages are not its own.
	stepStageRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`stepOf\(\s*(?:a\.steps|OWN)\s*,\s*'([A-Za-z0-9_-]+)'`)
	})
	// stepChildRe marks a script whose steps.child is a routed child's own steps (code-engineer → code-workers).
	stepChildRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\{\s*child:\s*CHILD_RAW\b`) })
)

// stepChildScript is the workflow whose stages steps.child names.
const stepChildScript = "code-workers.js"

// stepRoute previews an external stage's route; a test swaps it (production reads the registry and route state).
var stepRoute = func(who, model, profile, provider string) error {
	chain, res, err := agent.Resolve(context.Background(), agent.RouteReq{Agent: who, Model: model, Profile: profile,
		Provider: provider, Fallback: router.FallbackAgents, Preview: true})
	if err != nil {
		return err
	}
	if len(chain.Dispatches) == 0 && len(res.Candidates) == 0 {
		return fmt.Errorf("no candidate passes the gates (see `claude-code gateway --agent %s`)", who)
	}
	return nil
}

// checkWorkflowSteps lists every problem in args for script src (child scripts are read from dir); nil = may run.
func checkWorkflowSteps(src []byte, dir string, args any) []string {
	stages := scriptStages(string(src))
	if len(stages) == 0 {
		return nil // a stepOf-free script has no steps contract
	}
	a, ok := decodeArgs(args)
	if !ok && args != nil && args != "" {
		return []string{"args must be a JSON object (or a JSON-encoded object string)"}
	}
	run := stepRun{profile: plainString(a["profile"]), provider: plainString(a["provider"])}
	if stepChildRe().Match(src) {
		if child, err := os.ReadFile(filepath.Join(dir, stepChildScript)); err == nil {
			run.child = scriptStages(string(child))
			stages = append(stages, "child")
		}
	}
	return run.check("steps", a["steps"], stages)
}

// blockSteps is the Block naming every steps problem, or nil when there is none.
func blockSteps(what string, problems []string) *core.Result {
	if len(problems) == 0 {
		return nil
	}
	return &core.Result{Decision: core.Block, Reason: fmt.Sprintf("workflow-steps: blocked — %d problem(s) in %s's args; "+
		"fix them and re-run:\n  %s", len(problems), what, strings.Join(problems, "\n  "))}
}

// scriptStages is the sorted, de-duplicated stage names a script passes to stepOf for its own steps.
func scriptStages(src string) []string {
	var out []string
	for _, m := range stepStageRe().FindAllStringSubmatch(src, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// decodeArgs accepts args as an object or a JSON-encoded object string.
func decodeArgs(args any) (map[string]any, bool) {
	if s, ok := args.(string); ok {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) != nil {
			return nil, false
		}
		return m, true
	}
	m, ok := args.(map[string]any)
	return m, ok
}

func plainString(v any) string { s, _ := v.(string); return s }

// stepRun carries the run-wide profile/provider and the child's stages while one steps object is checked.
type stepRun struct {
	profile, provider string
	child             []string
}

// check validates one steps object against stages; "child" recurses into the child workflow's stages.
func (r stepRun) check(where string, steps any, stages []string) []string {
	if steps == nil {
		return nil
	}
	m, ok := steps.(map[string]any)
	if !ok {
		return []string{where + " must be an object"}
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch {
		case !slices.Contains(stages, k):
			out = append(out, fmt.Sprintf("%s.%s: unknown stage (allowed: %s)", where, k, strings.Join(stages, "|")))
		case k == "child" && r.child != nil:
			out = append(out, r.check(where+".child", m[k], r.child)...)
		default:
			out = append(out, r.stage(where+"."+k, m[k])...)
		}
	}
	return out
}

// stage validates one stage's fields, then previews an external stage's route.
func (r stepRun) stage(where string, v any) []string {
	m, ok := v.(map[string]any)
	if !ok {
		return []string{where + " must be an object"}
	}
	out := fieldProblems(where, m)
	if len(out) > 0 {
		return out
	}
	who := plainString(m["agent"])
	if who == "" || who == "claude" {
		return claudeStageProblems(where, m)
	}
	profile, provider := cmp.Or(plainString(m["profile"]), r.profile), cmp.Or(plainString(m["provider"]), r.provider)
	if err := stepRoute(who, plainString(m["model"]), profile, provider); err != nil {
		return []string{fmt.Sprintf("%s: %s has no route: %v", where, who, err)}
	}
	return nil
}

// fieldProblems checks field names and value shapes common to every stage.
func fieldProblems(where string, m map[string]any) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if p := fieldProblem(k, m[k]); p != "" {
			out = append(out, fmt.Sprintf("%s.%s%s", where, k, p))
		}
	}
	return out
}

// fieldProblem is the suffix describing what is wrong with one field, "" when it is fine.
func fieldProblem(k string, v any) string {
	s, isStr := v.(string)
	switch {
	case !slices.Contains(stepFields, k):
		return ": unknown field (allowed: " + strings.Join(stepFields, "|") + ")"
	case !isStr:
		return " must be a string"
	case k == "agent" && !slices.Contains(stepAgents, s):
		return " must be " + strings.Join(stepAgents, "|")
	case k == "effort" && !slices.Contains(stepEfforts, s):
		return " must be " + strings.Join(stepEfforts, "|")
	case k != "agent" && k != "effort" && !stepNameRe().MatchString(s):
		return " must be a plain name (no provider: prefix)"
	}
	return ""
}

// claudeStageProblems: an in-context claude stage runs on the session profile with a Claude alias.
func claudeStageProblems(where string, m map[string]any) []string {
	if m["profile"] != nil || m["provider"] != nil {
		return []string{where + ": claude stages run in-context on the session profile; " +
			"profile/provider need agent codex|copilot|gemini|opencode"}
	}
	if model := plainString(m["model"]); model != "" && !slices.Contains(stepClaudeModels, model) {
		return []string{fmt.Sprintf("%s.model must be %s", where, strings.Join(stepClaudeModels, "|"))}
	}
	return nil
}
