package dispatchhook

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
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/shell"
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
	tail := stripLabels(orig, st, label) // the task text, orig minus its leading labels and surrounding space
	oldHead := orig
	if i := strings.LastIndex(orig, tail); tail != "" && i >= 0 {
		oldHead = orig[:i] // the leading labels replaced; "" when none was stripped
	}
	out := lowerFirst(tail)
	if label != "" {
		if head, _, _ := cutHead(orig); keepsHead(head, label) { // else it stacks
			label = head // an equivalent label is kept as typed, so no rewrite is asked of auto mode
		}
		out = label + ": " + lowerFirst(tail)
	}
	if out == orig {
		return false, note
	}
	updated["description"] = out
	newHead := strings.TrimSuffix(out, lowerFirst(tail)) // the whole new prefix; both heads quoted exactly, no rune slice
	return true, strings.TrimSpace(note + "\n" + fmt.Sprintf("dispatch-effort: description head %q rewritten to %q.", oldHead, newHead))
}

// keepsHead: head already is label (sameLabel), so a resubmitted rewrite stays byte-identical.
func keepsHead(head, label string) bool {
	return labelShaped(strings.TrimSpace(head), label) && sameLabel(head, label)
}

// sameLabel: head is label, a tier word or a full id of its model, claiming `databricks-` only as label does.
func sameLabel(head, label string) bool {
	head = strings.TrimSpace(head)
	if head != "" && head == label {
		return true
	}
	tier := slices.Contains([]string{"haiku", "sonnet", "opus"}, strings.ToLower(head))        // a Claude tier word names its model
	if !tier && (!modelIDRe().MatchString(head) || !strings.ContainsAny(head, "0123456789")) { // a family or agent word
		return false
	}
	claimsDBX := strings.HasPrefix(strings.ToLower(head), models.DatabricksPrefix)
	if strings.Contains(head, "/") { // a provider-qualified head matches only the identical label
		return head == label
	}
	return head != "" && models.SameModel(head, label) && (!claimsDBX || strings.HasPrefix(label, models.DatabricksPrefix))
}

// stripLabels cuts every leading label-shaped `<head>:` so a doubled or wrong prefix is replaced, never stacked.
func stripLabels(desc, st, label string) string {
	for {
		head, rest, ok := cutHead(desc)
		head = strings.TrimSpace(head)
		if !ok || head == "" || (head != st && !labelShaped(head, label)) { // label "": a typed model head is unverified
			return strings.TrimSpace(desc)
		}
		desc = rest
	}
}

// cutHead is strings.Cut at the first colon, except that a legacy `auto (next: <id>)` head is one head.
func cutHead(desc string) (head, rest string, ok bool) {
	if t := strings.TrimLeft(desc, " "); strings.HasPrefix(t, legacyAutoHead) {
		if i := strings.Index(t, ")"); i >= 0 {
			if after, found := strings.CutPrefix(strings.TrimLeft(t[i+1:], " "), ":"); found {
				return t[:i+1], after, true
			}
		}
	}
	return strings.Cut(desc, ":")
}

// modelIDRe matches a model id: no spaces, a letter, a digit and a `-` or `/` ("gpt-6.1-sol", "opencode-go/kimi-k3").
var modelIDRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z][\w.]*[-/][\w./-]*$`) })

const legacyAutoHead = "auto (next:"

// labelShaped: head is the label, `auto`, a legacy `auto (next: <id>)`, a model id, or a retired `(<id>)`.
func labelShaped(head, label string) bool {
	if head == label || head == agentpkg.AutoModel || (strings.HasPrefix(head, legacyAutoHead) && strings.HasSuffix(head, ")")) {
		return true
	}
	if in, ok := strings.CutPrefix(head, "("); ok {
		head = strings.TrimSuffix(in, ")")
	}
	family := slices.Contains(strings.FieldsFunc(strings.ToLower(label), func(r rune) bool { return r == '-' || r == '/' }),
		strings.ToLower(head)) // a bare word of the label's own id ("gpt" of "gpt-6.1-sol") is a short label, never task text
	return family || slices.Contains(models.AliasNames, head) || agentid.Known(head) ||
		(modelIDRe().MatchString(head) && strings.ContainsAny(head, "0123456789"))
}

// ownModel is the dispatch's model, else the agent file's, else (also for `inherit`) the session's; "" labels nothing.
func ownModel(ctx context.Context, run *dispatchRun, updated map[string]any, st string) string {
	m, _ := updated["model"].(string)
	if m == "" || m == "inherit" {
		m = run.rosterModel(st)
	}
	if m == "" || m == "inherit" {
		return run.inheritedID(run.session())
	}
	if !slices.Contains(models.AliasNames, m) {
		if reg, err := run.registry(); err == nil {
			if p := reg.Row(providers.SessionLeadContext(ctx, reg, true)); p != nil {
				return p.WireModel(m)
			}
		}
		return m
	}
	return run.databricksID(m)
}

// inheritedID is the session model's label; a Fable session id on a Databricks route inherits that route's Opus (D11).
func (r *dispatchRun) inheritedID(session string) string {
	id := r.databricksID(session)
	if c, _, ok := models.Resolve(id); !ok || models.FamilyOf(c) != models.Fable {
		return id
	}
	if reg, p, _ := r.servingRow(); p != nil {
		return opusPin(reg, p)
	}
	return id
}

// servingRow is the run's profile (--profile, else the session's routed one) when it is a databricks-serving row.
func (r *dispatchRun) servingRow() (*providers.Registry, *providers.Provider, string) {
	name, routed := providers.SessionName()
	if r.profile != "" {
		name, routed = r.profile, true
	}
	reg, err := r.registry()
	if err != nil || !routed {
		return nil, nil, ""
	}
	if p := reg.Row(name); p != nil && p.SpellingRule() == "databricks-serving" {
		return reg, p, name
	}
	return nil, nil, ""
}

// databricksID is a Claude alias or id spelled as its `databricks-` id on a Databricks-routed session; else m as is.
func (r *dispatchRun) databricksID(m string) string {
	name, routed := providers.SessionName() // routed comes from a loopback ANTHROPIC_BASE_URL /p/<name>
	if !routed || m == "" {
		return m
	}
	reg, err := r.registry()
	if err != nil {
		return m
	}
	if p := reg.Row(name); p != nil && p.SpellingRule() == "databricks-serving" {
		return p.WireModel(m)
	}
	return m
}

// workAgent is the external agent doing the work ("" = Claude): the type's agent, or for claude its Agent: line's one.
func workAgent(st, task string) string {
	if externalTypes[st] {
		return typeAgent(st)
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
		if externalAgent(a) {
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
	model, signal := lookupPin(effectiveType(updated), task)
	if model == "" {
		model, signal = agentpkg.TaskTier(task)
	}
	if model == "" {
		return "", ""
	}
	updated["model"] = model
	tier := agentpkg.TierEffort(model)
	effort := pinEffort(run, in, updated, tier)
	return fmt.Sprintf("dispatch-effort: model unset, task classifies %s: pinned model=%s%s (set model explicitly to override).", tier, model, effort),
		fmt.Sprintf("dispatch-effort: %s → %s%s (%s)", tier, model, effort, signal)
}

// lookupPin is haiku for an Explore sweep or a read-only general-purpose/claude-code-guide lookup; "" keeps TaskTier.
func lookupPin(st, task string) (model, signal string) {
	switch st {
	case "Explore":
		return agentpkg.LookupTier(task, false)
	case "", "general-purpose", "claude-code-guide":
		return agentpkg.LookupTier(task, true)
	}
	return "", ""
}

// pinEffort sets updated["effort"] to tier unless the call or agent file sets one; returns " effort=<tier>", else "".
func pinEffort(run *dispatchRun, in *core.Input, updated map[string]any, tier string) string {
	if strings.TrimSpace(in.Nested("tool_input", "effort")) != "" || run.rosterEffort(effectiveType(updated)) != "" {
		return ""
	}
	updated["effort"] = tier
	return " effort=" + tier
}

// effectiveType is the subagent_type the dispatch will run as, after any retype.
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
	r, ok, _, err := labelRoute(ctx, run, updated, st, task)
	label = r.Label()
	switch {
	case err != nil:
		return "", fmt.Sprintf("dispatch-effort: no description label: the %s route does not resolve offline (%v); "+
			"check it with `claude-code gateway resolve %s`.", r.Agent, err, r.Agent)
	case !ok:
		return "", ""
	case r.Note != "":
		note = fmt.Sprintf("dispatch-effort: label %s: %s.", label, r.Note)
	}
	return label, strings.TrimSpace(note + "\n" + run.fableNote(r, label) + "\n" + googleQuotaNote(ctx, r))
}

// fableNote says a Fable label on a Databricks route goes to first-party Anthropic; the call is unchanged.
func (r *dispatchRun) fableNote(route agentpkg.ModelRoute, label string) string {
	if c, _, ok := models.Resolve(label); !ok || models.FamilyOf(c) != models.Fable || (route.Agent != "" && r.profile == "") {
		return "" // an external agent's own route is not the session's
	}
	_, p, name := r.servingRow()
	if p == nil {
		return ""
	}
	return fmt.Sprintf("dispatch-effort: label %s: profile %s serves no Fable; the proxy sends this call to first-party "+
		"Anthropic, billed there.", label, name)
}

// opusPin is the profile's Opus id: the session pin, else the wire spelling.
func opusPin(reg *providers.Registry, p *providers.Provider) string {
	if sel, err := providers.Select(reg, p.Name); err == nil {
		if c, _, ok := models.Resolve(sel.ClientModel(models.Opus)); ok && models.FamilyOf(c) == models.Opus {
			return sel.ClientModel(models.Opus)
		}
	}
	return p.WireModel(models.Opus)
}

// googleQuotaCheck is the agy /usage seam, cache-only so a label never waits on an agy spawn; tests stub it.
var googleQuotaCheck = gemini.CheckCached

// googleQuotaNote is the low-pool warning and a held model's reset for a gemini dispatch on Google; "" otherwise.
func googleQuotaNote(ctx context.Context, r agentpkg.ModelRoute) string {
	if r.Agent != agentid.Gemini || !router.PerModelQuota(r.Account) {
		return ""
	}
	now := time.Now()
	q, err := googleQuotaCheck(ctx, r.Model, now)
	if err != nil {
		return ""
	}
	var lines []string
	if w := q.Warning(); w != "" {
		lines = append(lines, "dispatch-effort: "+w)
	}
	if q.HeldUntil.After(now) {
		lines = append(lines, fmt.Sprintf("dispatch-effort: %s is held until %s (local); the ask will refuse it.",
			r.Model, q.HeldUntil.Local().Format("2006-01-02 15:04")))
	}
	return strings.Join(lines, "\n")
}

// labelQuery is the resolver query for the dispatch: leaf type, model pin and --profile.
func labelQuery(run *dispatchRun, updated map[string]any, st, task string) (agentpkg.ModelQuery, bool) {
	agent := workAgent(st, task)
	q := agentpkg.ModelQuery{Agent: cmp.Or(agent, agentid.Claude), Profile: run.profile}
	q.Provider, q.Profile = askRoute(agent, q.Profile)
	if m, _ := updated["model"].(string); m != "" && !slices.Contains(models.AliasNames, models.Strip1M(m)) && m != "inherit" {
		q.Model = m // an exact dispatch model, spelled as a --model ask runs it
	}
	if q.Model == "" && agent != "" && run.model != "inherit" {
		q.Model = run.model // the verb's --model is the ask's, an alias included
	}
	if q.Model == "" && agent == "" && q.Profile == "" {
		return q, false // an in-context leaf: its own model
	}
	q.Task = task
	if q.Model == "" {
		q.Default, _ = agentpkg.TaskTier(q.Task) // the ask runs its pin, else the task tier
	}
	return q, true
}

// askRoute is the agent file's baked --provider and --profile for the leaf's work agent, unless a profile is typed.
func askRoute(agent, profile string) (provider, prof string) {
	if profile != "" || agent == "" {
		return "", profile
	}
	return bakedRoute(agent)
}

// bakedRoute is the route of the agent's own `agents ask` line in its type's file; the launch repo's dir is read first.
func bakedRoute(agent string) (provider, profile string) {
	name := agentid.SubagentType(agent)
	if _, ok := agentFile(name); !ok {
		return "", ""
	}
	dirs := []string{filepath.Join(paths.ConfigDir(), "agents")}
	if pdir := projectAgentsDir(); pdir != "" {
		dirs = slices.Insert(dirs, 0, pdir)
	}
	for _, dir := range dirs {
		raw, err := readAgentFile(filepath.Join(dir, name+".md"))
		if err != nil {
			continue
		}
		return askLineRoute(string(raw), agent)
	}
	return "", ""
}

// askLine keeps the same flag precedence as the run it predicts.
type askLine struct{ Agent, Provider, Profile, Model, Tier string }

// askLineRoute is the route of the first `agents ask --agent <agent>` line in text, continuations joined.
func askLineRoute(text, agent string) (provider, profile string) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\\\r\n", " "), "\\\n", " ")
	for line := range strings.SplitSeq(text, "\n") {
		words := quotedFields(line)
		if len(words) < 4 || words[0] != "claude-code" || words[1] != "agents" || words[2] != "ask" {
			continue
		}
		if l, ok := readAskLine(words[3:]); ok && l.Agent == agent {
			return l.Provider, l.Profile
		}
	}
	return "", ""
}

// readAskLine refuses unreadable overlay values without loading files or resolving routes.
func readAskLine(words []string) (askLine, bool) {
	a, ok := ParseAskWords(words)
	if !ok {
		return askLine{}, false
	}
	if a.Has("overlay") && !a.overlayOK {
		return askLine{}, false
	}
	return askLine{a.Value("agent"), a.Value("provider"), a.Value("profile"), a.Value("model"), a.Value("tier")}, true
}

// quotedFields shares shell unquoting with the receipt reader and refuses an unterminated quote or escape.
func quotedFields(line string) []string {
	line = shell.DropComments(line)
	if at := shell.QuoteAt(line + "\x00"); at[len(at)-1] != 0 {
		return nil
	}
	return shell.Tokens(line)
}

// labelRoute resolves labelQuery the way the run does; ok is false when the label is the leaf's own model.
// auto is true when the run picks its own model.
func labelRoute(ctx context.Context, run *dispatchRun, updated map[string]any, st, task string) (r agentpkg.ModelRoute, ok, auto bool, err error) {
	q, ok := labelQuery(run, updated, st, task)
	if !ok {
		return agentpkg.ModelRoute{Model: ownModel(ctx, run, updated, st)}, true, false, nil
	}
	reg, err := run.registry()
	if err == nil {
		err = providers.CheckModel(reg, q.Model)
	}
	if err != nil {
		return agentpkg.ModelRoute{Agent: q.Agent}, false, false, err
	}
	hint := router.RuleClassifier{}.Alias(task)
	hop, note, err := run.runHop(ctx, reg, q, hint)
	if err != nil { // %v: an offline ladder miss is no ErrModelRoute, so preflight never denies on it
		return agentpkg.ModelRoute{Agent: q.Agent}, false, false, fmt.Errorf("%v", err)
	}
	r, choice, err := resolveModel(reg, agentpkg.QueryFor(agentpkg.PeekContext(ctx), hop), time.Now())
	if err != nil {
		return agentpkg.ModelRoute{Agent: hop.Agent}, false, false, err
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	auto = agentpkg.AutoRun(ctx, hop)
	if auto && choice != nil && choice.Fallback != "" {
		r.Model = agentpkg.AutoModel // no peek names the pick: never the static pin
	}
	return r, true, auto, nil
}

// resolveModel (ExplainRun) and runQuery are seams: tests stub them so a label never reads the live vault.
var (
	resolveModel = agentpkg.ExplainRun
	runQuery     = agentpkg.RunQuery
)
