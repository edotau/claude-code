package hooks

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
)

// applyLabel rewrites the description to `<model>: <task>`; note names the rewrite or the route it could not resolve.
func applyLabel(ctx context.Context, run *dispatchRun, updated map[string]any, task string) (changed bool, note string) {
	st, _ := updated["subagent_type"].(string)
	orig, _ := updated["description"].(string)
	if strings.TrimSpace(orig) == "" {
		return false, ""
	}
	label, note := labelModel(ctx, run, updated, st, task)
	rest := stripLabels(orig, st, label)
	out := lowerFirst(rest)
	if label != "" {
		out = label + ": " + out
	}
	if out == orig {
		return false, note
	}
	updated["description"] = out
	return true, strings.TrimSpace(note + "\n" + fmt.Sprintf("dispatch-effort: description %q rewritten to %q.", orig, out))
}

// stripLabels cuts every leading label-shaped `<head>:` so a doubled or wrong prefix is replaced, never stacked.
func stripLabels(desc, st, label string) string {
	for {
		head, rest, ok := strings.Cut(desc, ":")
		head = strings.TrimSpace(head)
		if !ok || head == "" || (head != st && (label == "" || !labelShaped(head, label))) {
			return strings.TrimSpace(desc)
		}
		desc = rest
	}
}

// modelIDRe matches a model id: no spaces, a letter, a digit and a `-` or `/` ("gpt-6.1-sol", "opencode-go/kimi-k3").
var modelIDRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z][\w.]*[-/][\w./-]*$`) })

func labelShaped(head, label string) bool {
	switch {
	case head == label || slices.Contains(models.AliasNames, head) || agentid.Known(head):
		return true
	}
	return modelIDRe().MatchString(head) && strings.ContainsAny(head, "0123456789")
}

// ownModel is the dispatch's model, else the agent file's, else (also for `inherit`) the session's; "" labels nothing.
func ownModel(ctx context.Context, run *dispatchRun, updated map[string]any, st string) string {
	m, _ := updated["model"].(string)
	if m == "" || m == "inherit" {
		m = run.rosterModel(st)
	}
	if m == "" || m == "inherit" {
		return run.session()
	}
	if !slices.Contains(models.AliasNames, m) {
		if reg, err := run.registry(); err == nil {
			if p := reg.Row(providers.SessionLeadContext(ctx, reg, true)); p != nil {
				return p.WireModel(m)
			}
		}
	}
	return m
}

// workAgent is the external agent doing the work ("" = Claude): the type, or for claude its Agent: line's one agent.
func workAgent(st, task string) string {
	if externalTypes[st] {
		return st
	}
	if st != agentid.Claude {
		return ""
	}
	return briefExternal(task)
}

// briefExternal is the one external agent the brief's Agent: line names; "" for none or several.
func briefExternal(task string) string {
	var named []string
	for _, a := range briefAgents(task) {
		if externalTypes[a] {
			named = append(named, a)
		}
	}
	if len(named) != 1 {
		return ""
	}
	return named[0]
}

// pinModel fills an unset updated["model"] from the classifier; returns the note and visible reason ("" = no pin).
func pinModel(run *dispatchRun, in *core.Input, updated map[string]any, task string) (note, systemMessage string) {
	// never override an explicit model or a roster pin; `inherit` means unset (honoring it ran on the parent's model)
	if explicitModel(in) || task == "" || agentPinsModel(run, effectiveType(updated)) {
		return "", ""
	}
	model, signal := agentpkg.TaskTier(task)
	if model == "" {
		return "", ""
	}
	updated["model"] = model
	tier := agentpkg.TierEffort(model)
	effort := ""
	if set := pinEffort(run, in, updated, tier); set != "" {
		effort = " effort=" + set
	}
	return fmt.Sprintf("dispatch-effort: dispatch model unset and the task classifies %s — pinned model=%s%s"+
			" (set model explicitly to override).", tier, model, effort),
		fmt.Sprintf("dispatch-effort: %s → %s%s (%s)", tier, model, effort, signal)
}

// pinEffort sets updated["effort"] to tier unless the call or agent file sets one; returns what it set, else "".
func pinEffort(run *dispatchRun, in *core.Input, updated map[string]any, tier string) string {
	if strings.TrimSpace(in.Nested("tool_input", "effort")) != "" || run.rosterEffort(effectiveType(updated)) != "" {
		return ""
	}
	updated["effort"] = tier
	return tier
}

// effectiveType is the subagent_type the dispatch will run as, after any coworker retype.
func effectiveType(updated map[string]any) string {
	st, _ := updated["subagent_type"].(string)
	return st
}

// explicitModel is a dispatch-set model other than `inherit` (the default spelling for unset).
func explicitModel(in *core.Input) bool {
	m := strings.TrimSpace(in.Nested("tool_input", "model"))
	return m != "" && !strings.EqualFold(m, "inherit")
}

// agentFile is the agents/<name>.md file of an agent type; ok is false for an empty or path-shaped name.
func agentFile(name string) (string, bool) {
	if name == "" || filepath.Base(name) != name {
		return "", false
	}
	return filepath.Join(paths.ConfigDir(), "agents", name+".md"), true
}

// rosterEntry is the named harness agent's frontmatter model and effort ("" for a built-in or a path-shaped name).
func rosterEntry(name string) (model, effort string) {
	file, ok := agentFile(name)
	if !ok {
		return "", ""
	}
	raw, err := readAgentFile(file)
	if err != nil {
		return "", ""
	}
	fm, _ := textutil.ParseFrontmatter(string(raw))
	model, _ = fm["model"].(string)
	effort, _ = fm["effort"].(string)
	return model, effort
}

// agentPinsModel reports whether the named subagent's frontmatter sets any model, `inherit` included.
func agentPinsModel(run *dispatchRun, name string) bool {
	if name == "" {
		return false // no subagent_type = a fresh general-purpose agent, nothing to defer to
	}
	if _, ok := agentFile(name); !ok {
		return true // path-shaped input is not a roster name; refuse to touch the dispatch
	}
	return run.rosterModel(name) != "" // no roster file = a built-in (Explore/Plan), the classifier may apply
}

// labelModel is the model the dispatch's ask runs (the resolver's wire id, else the own model) and the route's note.
func labelModel(ctx context.Context, run *dispatchRun, updated map[string]any, st, task string) (label, note string) {
	r, ok, err := labelRoute(ctx, run, updated, st, task)
	switch {
	case err != nil:
		return "", fmt.Sprintf("dispatch-effort: no description label: the %s route does not resolve offline (%v); "+
			"check it with `claude-code gateway resolve %s`.", r.Agent, err, r.Agent)
	case !ok:
		return "", ""
	case r.Note != "":
		note = fmt.Sprintf("dispatch-effort: label %s: %s.", r.Label(), r.Note)
	}
	return r.Label(), note
}

// labelQuery is the resolver query for the dispatch: the Run: line's flags and overlay, as `agents ask` reads them.
func labelQuery(ctx context.Context, run *dispatchRun, updated map[string]any, st, task string) (agentpkg.ModelQuery, bool) {
	line := run.opening(ctx, task)
	agent := line.workAgent(st, task)
	q := agentpkg.ModelQuery{Agent: cmp.Or(agent, agentid.Claude), Model: cmp.Or(line.model, line.cfg.Model),
		Profile: cmp.Or(line.profile, line.cfg.Profile, run.profile), Provider: line.cfg.Provider, Account: line.cfg.Account,
		Effort: line.cfg.Effort, Identity: line.cfg.Identity, Fallback: cmp.Or(line.fallback, line.cfg.Fallback)}
	if m, _ := updated["model"].(string); q.Model == "" && m != "" && !slices.Contains(models.AliasNames, models.Strip1M(m)) && m != "inherit" {
		q.Model = m // an exact dispatch model, spelled as a --model ask runs it
	}
	if q.Model == "" && agent == "" && q.Profile == "" && q.Provider == "" {
		return q, false // an in-context leaf: its own model
	}
	q.Task, q.Tools = cmp.Or(line.prompt, task), line.tools // auto and the task tier size the ask's prompt
	if q.Model == "" {
		q.Default, _ = agentpkg.TaskTier(q.Task) // the ask runs its pin, else the task tier
	}
	return q, true
}

// labelRoute resolves labelQuery the way the run does; ok is false when the label is the leaf's own model.
func labelRoute(ctx context.Context, run *dispatchRun, updated map[string]any, st, task string) (agentpkg.ModelRoute, bool, error) {
	q, ok := labelQuery(ctx, run, updated, st, task)
	if !ok {
		return agentpkg.ModelRoute{Model: ownModel(ctx, run, updated, st)}, true, nil
	}
	reg, err := run.registry()
	if err != nil {
		return agentpkg.ModelRoute{Agent: q.Agent}, false, err
	}
	hint := router.RuleClassifier{}.Alias(cmp.Or(run.opening(ctx, task).prompt, task)) // ask classifies its prompt
	hop, note, err := runQuery(ctx, reg, q, hint)
	if err != nil { // %v: an offline ladder miss is no ErrModelRoute, so preflight never denies on it
		return agentpkg.ModelRoute{Agent: q.Agent}, false, fmt.Errorf("%v", err)
	}
	r, err := resolveModel(reg, agentpkg.QueryFor(ctx, hop), time.Now())
	if err != nil {
		return agentpkg.ModelRoute{Agent: hop.Agent}, false, err
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, true, nil
}

// resolveModel and runQuery are seams: tests stub them so a label never reads the live vault or route table.
var (
	resolveModel = agentpkg.ResolveRun
	runQuery     = agentpkg.RunQuery
)
