package ask

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func init() { askspec.Register(askSchema) }

// askSchema is ask's FlagSet as spec keys; choices come from the same values the validators check.
func askSchema() askspec.Schema {
	fs, _ := askFlagSet()
	return askspec.FromFlagSet(fs, askspec.Opts{
		Choices: map[string][]string{"fallback": route.FallbackModes,
			"format": agent.Formats, "effort": agentid.EffortNames(), "as": {"auto", "user", "sp"}, "codemode": route.Codemodes, "runner": route.Runners, "tier": agent.TierNames()},
		JSON: []string{"overlay", "agents", "output-schema"},
	})
}

func specParts(value string, stdin io.Reader) ([]string, string, askspec.Task, error) {
	raw, err := agentconfig.ReadJSONSource("spec", value, stdin)
	if err != nil {
		return nil, "", askspec.Task{}, err
	}
	if raw, err = settingsKeys(raw); err != nil {
		return nil, "", askspec.Task{}, err
	}
	s, _ := askspec.Current()
	return s.Argv(raw)
}

// settingsKeys spells a spec in the record's names (the vault record) as ask's flag keys; flag keys pass through.
func settingsKeys(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || (doc["fallback_mode"] == nil && doc["route"] == nil) {
		return raw, nil // not an object, or no renamed key: Argv reports it
	}
	if v, ok := doc["fallback_mode"]; ok {
		delete(doc, "fallback_mode")
		doc["fallback"] = v
	}
	if v, ok := doc["route"]; ok {
		delete(doc, "route")
		if err := routeKeys(doc, v); err != nil {
			return nil, fmt.Errorf("spec route: %w", err)
		}
	}
	return json.Marshal(doc)
}

// routeFlags maps a spec route key to the flag key it types; an empty value reaches that flag's validator.
var routeFlags = map[string]string{"provider": "provider", "profile": "profile", "identity": "as",
	"agent": "agent", "runner": "runner", "fallback": "fallback"}

// routeIgnored are the keys route.Route has but a spec never applied; they and a null are noted, not refused.
var routeIgnored = []string{"account", "host", "remote"}

// routeKeys adds the spec route's keys to doc as flag keys; an unknown key fails, an ignored one is noted on stderr.
func routeKeys(doc map[string]json.RawMessage, v json.RawMessage) error {
	var r map[string]json.RawMessage
	if err := json.Unmarshal(v, &r); err != nil {
		return errors.New("want an object")
	}
	if r == nil {
		specIgnored("route")
	}
	for _, k := range slices.Sorted(maps.Keys(r)) {
		if err := routeKey(doc, k, r[k]); err != nil {
			return err
		}
	}
	return nil
}

// routeKey adds one spec route key to doc as its flag key.
func routeKey(doc map[string]json.RawMessage, k string, raw json.RawMessage) error {
	flagKey, ok := routeFlags[k]
	var val string
	switch {
	case string(raw) == "null" || slices.Contains(routeIgnored, k):
		specIgnored("route." + k)
		return nil
	case !ok:
		return fmt.Errorf("%s: unknown key (%s)", k, strings.Join(slices.Sorted(maps.Keys(routeFlags)), ", "))
	case json.Unmarshal(raw, &val) != nil:
		return fmt.Errorf("%s: want a string", k)
	}
	if k == "profile" && val == route.AutoArm { // the pool picks: a provider alone
		return nil
	}
	setRouteFlag(doc, k, flagKey, val)
	return nil
}

// setRouteFlag stores val under flagKey; top-level agent, runner and fallback win, and an empty flag value is skipped.
func setRouteFlag(doc map[string]json.RawMessage, k, flagKey, val string) {
	if flagKey == "agent" || flagKey == "runner" || flagKey == "fallback" {
		if doc[flagKey] != nil {
			fmt.Fprintf(os.Stderr, "spec: route.%s is ignored (the top-level %s wins)\n", k, flagKey)
			return
		}
	} else if val == "" {
		return
	}
	doc[flagKey], _ = json.Marshal(val)
}

func specIgnored(key string) {
	fmt.Fprintf(os.Stderr, "spec: %s is ignored (it has no effect)\n", key)
}

// specBrief is prompt as the leaf brief the own, report and verify keys describe; no contract returns it as is.
func specBrief(prompt string, argv []string, t askspec.Task) (string, *contractOpen, error) {
	if !t.Set() {
		return prompt, nil, nil
	}
	if t.Verify != "" && plans.VerifyShape(t.Verify) != nil {
		return "", nil, errors.New("spec verify: want go build|vet|test on ./ packages, agents diff --check or doctor, " +
			"rtk forms, joined by &&")
	}
	if strings.ContainsAny(t.Report, "\r\n") {
		return "", nil, errors.New("spec report: want a one-line path")
	}
	bf, err := briefTarget(argv)
	if err != nil {
		return "", nil, err
	}
	var o *contractOpen
	if t.Contract != nil {
		if o, err = newContractOpen(t, bf); err != nil {
			return "", nil, err
		}
	}
	brief, err := agent.RenderBrief(agent.BriefInput{Task: prompt, Agent: bf.name, Owned: t.Own, Report: t.Report,
		Verify: t.Verify, PlanTask: bf.planTask, Tier: bf.tier, CodeMode: &bf.codeMode})
	if err == nil && o != nil {
		brief += contractBlock(o)
	}
	return brief, o, err
}

// briefFlags is what the merged argv dispatches, as the brief renders it.
type briefFlags struct {
	name, planTask, tier string
	workdir              string
	codeMode             bool
}

// briefTarget is the agent, code mode, plan task and tier the merged argv dispatches, resolved as the real run will.
func briefTarget(argv []string) (briefFlags, error) {
	fs, f := askFlagSet()
	fs.SetOutput(io.Discard)
	_, _ = helptext.ParseArgs(fs, argv) // the real parse reports a bad flag
	if f.batchSrc != "" || f.loop || f.live || f.waves != "" || f.retry != "" || f.job != "" {
		return briefFlags{}, errors.New("spec own, report, verify and contract are for one new dispatch: not with --batch, --loop, " +
			"--live, --waves, --retry or --job")
	}
	defaultTools(fs, f)
	name := f.name
	if defs, err := parseAgentDefs(string(f.agents), f.host); err == nil {
		if ag, _, err := agent.SelectAgent(name, defs); err == nil {
			name = ag
		}
	}
	mode, _, _ := validateCodemode(f.codemode, name, "", f.tools, directRoute(f.provider, f.profile))
	return briefFlags{name: name, planTask: f.planTask, tier: f.tier, workdir: f.workdir, codeMode: mode != "" && mode != "off"}, nil
}

// takeAction stores the spec's no-model action on f; it refuses any task text.
func takeAction(f *askFlags, task askspec.Task, prompt string, pos []string) error {
	if len(pos) > 0 || strings.TrimSpace(prompt) != "" || task.Report != "" || task.Verify != "" || task.Contract != nil {
		return errors.New("spec action runs no model: drop the prompt, task words, report, verify and contract (own and agent/model stay)")
	}
	f.action, f.actionOwn = task.Action, task.Own
	return nil
}

// withSpec puts the spec's argv first, so a typed scalar flag wins and a typed list adds to the spec's.
func withSpec(f *askFlags, args, pos []string) ([]string, error) {
	argv, prompt, task, err := specParts(f.spec, os.Stdin)
	if err != nil {
		return nil, err
	}
	out := append(argv, dropSpec(args)...)
	if task.Action != "" {
		return out, takeAction(f, task, prompt, pos)
	}
	return withPrompt(f, out, args, pos, prompt, task)
}

// withPrompt appends the spec's brief (or inline prompt) to out unless a positional task was typed.
func withPrompt(f *askFlags, out, args, pos []string, prompt string, task askspec.Task) ([]string, error) {
	var err error
	if task.Set() && (len(pos) > 0 || strings.TrimSpace(prompt) == "" || prompt == "-") {
		return nil, errors.New("spec own, report, verify and contract wrap the spec's own inline prompt: set prompt, no positional task")
	}
	if prompt, f.contract, err = specBrief(prompt, out, task); err != nil {
		return nil, err
	}
	f.verify = task.Verify
	if prompt != "" && len(pos) == 0 {
		if _, term := walkArgs(args); term < 0 {
			out = append(out, "--")
		}
		out = append(out, prompt)
	}
	return out, nil
}

// runWaves runs --waves unless a typed or overlay host is set: the wave runner dispatches leaves on this machine.
func runWaves(f *askFlags, raw []string) int {
	if f.host == "" && f.overlay != "-" {
		ov, err := LoadOverlay(context.Background(), f.overlay, "")
		if err != nil {
			return failAskCode(f.format, f.name, err, 2)
		}
		f.host = ov.Host
	}
	if f.host == "" {
		return Waves(raw)
	}
	err := errors.New("--waves cannot take --host: the wave runner dispatches leaves on this machine (use --remote to run the whole waves on a host)")
	return failAskCode(f.format, f.name, err, 2)
}

// parseAsk parses args, re-parses them with --spec's argv first, then refuses a leading agent word.
func parseAsk(args []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	fs, f := askFlagSet()
	pos, code, done := parseAskArgs(fs, args)
	raw := args
	if !done && f.spec != "" {
		fs, f, args, pos, code, done = reparseSpec(f, args, pos)
	}
	if !done && f.waves != "" && f.action == "" { // an action's own refusal names --waves
		return fs, f, raw, pos, runWaves(f, raw), true
	}
	if done {
		return fs, f, args, pos, code, true
	}
	if err := refuseAgentWord(fs, args, pos); err != nil {
		return fs, f, args, pos, failAskCode(f.format, f.name, err, 2), true
	}
	return fs, f, args, pos, 0, false
}

// reparseSpec re-parses args with the spec's argv first on a fresh FlagSet; f.spec survives for stdinTaken.
func reparseSpec(f *askFlags, args, pos []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	merged, err := withSpec(f, args, pos)
	if err != nil {
		return nil, f, args, nil, failAskCode(f.format, f.name, err, 2), true
	}
	fs, nf := askFlagSet()
	pos, code, done := parseAskArgs(fs, merged)
	nf.spec, nf.verify, nf.contract = f.spec, f.verify, f.contract
	nf.action, nf.actionOwn = f.action, f.actionOwn
	return fs, nf, merged, pos, code, done
}

// refuseAgentWord rejects a first task word that is exactly an agent name when --agent was not given; "--" opts out.
func refuseAgentWord(fs *flag.FlagSet, args, pos []string) error {
	if w, ok := askspec.LeadingAgentWord(args, pos, agent.RosterNames(), helptext.Explicit(fs, "agent")); ok {
		return fmt.Errorf("%q is an agent name; use --agent %s (or -- before a prompt that starts with it)", w, w)
	}
	return nil
}

// walkArgs finds each --spec word (and its value) and the real "--" terminator, skipping other flags' values.
func walkArgs(args []string) (drop map[int]bool, term int) {
	fs, _ := askFlagSet()
	drop, term = map[int]bool{}, -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return drop, i
		}
		if len(a) < 2 || a[0] != '-' {
			continue
		}
		name, _, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "spec" {
			drop[i] = true
			if !hasVal && i+1 < len(args) {
				i++
				drop[i] = true
			}
			continue
		}
		if takesValue(fs.Lookup(name)) && !hasVal {
			i++
		}
	}
	return drop, term
}

func takesValue(f *flag.Flag) bool {
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// dropSpec removes --spec V and --spec=V so the re-parse does not read the spec twice.
func dropSpec(args []string) []string {
	drop, _ := walkArgs(args)
	var out []string
	for i, a := range args {
		if !drop[i] {
			out = append(out, a)
		}
	}
	return out
}

// canonicalArgv is the typed flags in name order plus the task: the command a re-run types.
func canonicalArgv(fs *flag.FlagSet, lists map[string][]string, prompt string, fromStdin bool, overlayArg *string, skip ...string) []string {
	argv := []string{"claude-code", "agents", "ask"}
	fs.Visit(func(f *flag.Flag) {
		if canonicalArgvSkip(f.Name) || slices.Contains(skip, f.Name) {
			return
		}
		argv = append(argv, canonicalArgvFlag(f, overlayArg, lists)...)
	})
	if fromStdin {
		return append(argv, "-")
	}
	return append(argv, "--", prompt)
}

func canonicalArgvSkip(name string) bool { return name == "spec" || name == "dry-run" }

// listValues is each unsplit repeatable flag's elements: a value may hold a comma, so String()+SplitCSV would split it.
func (f *askFlags) listValues() map[string][]string {
	return map[string][]string{"env": f.env}
}

func canonicalArgvFlag(f *flag.Flag, overlayArg *string, lists map[string][]string) []string {
	if f.Name == "overlay" && overlayArg != nil {
		return canonicalOverlayFlag(f.Name, *overlayArg)
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return canonicalBoolFlag(f.Name, f.Value.String())
	}
	if l, ok := f.Value.(interface{ IsRepeatable() bool }); ok && l.IsRepeatable() {
		vals, held := lists[f.Name]
		if !held {
			vals = textutil.SplitCSV(f.Value.String())
		}
		var out []string
		for _, v := range vals {
			out = append(out, "--"+f.Name, v)
		}
		return out
	}
	return []string{"--" + f.Name, f.Value.String()}
}

func canonicalOverlayFlag(name, value string) []string {
	if value == "" {
		return nil
	}
	return []string{"--" + name, value}
}

func canonicalBoolFlag(name, value string) []string {
	if value == "true" {
		return []string{"--" + name}
	}
	return []string{"--" + name + "=false"}
}

// dryCombos refuses --dry-run with a run shape it cannot preview.
func dryCombos(f *askFlags) error {
	if f.dryRun && (f.live || f.batchSrc != "" || f.job != "") {
		return errors.New("--dry-run cannot be combined with --live, --batch or --job")
	}
	return nil
}

// dryCtx is ctx carrying the dry doc (overlay, argv) when --dry-run is set.
func (r *askRun) dryCtx(ctx context.Context, prompt string) context.Context {
	if !r.f.dryRun {
		return ctx
	}
	if r.f.profile == "" && r.typed != "" { // resolveLead moved the typed profile into the lead; the re-run types it
		_ = r.fs.Set("profile", r.typed)
	}
	if r.f.provider == "" && r.typedProv != "" {
		_ = r.fs.Set("provider", r.typedProv)
	}
	if fr := flagRoute(r.f); r.f.host == "" && r.f.provider == "" && r.f.profile != "" && fr.Provider != "" { // the re-run types the pair
		_ = r.fs.Set("provider", fr.Provider)
	}
	overlayArg := r.canonicalOverlayArg()
	argv := canonicalArgv(r.fs, r.f.listValues(), prompt, taskFromStdin(r.pos), overlayArg)
	return context.WithValue(ctx, dryKey{}, dryDoc{Overlay: r.ov, Argv: textutil.ShellJoin(argv), Terms: r.f.contract.terms()})
}

func (r *askRun) canonicalOverlayArg() *string {
	if !r.flagRoute || !helptext.Explicit(r.fs, "overlay") {
		return nil
	}
	ov := r.ov
	ov.Schema = 0
	if ov.Empty() && !ov.Routed() {
		value := ""
		return &value
	}
	raw, err := json.Marshal(ov)
	if err != nil {
		return nil
	}
	value := string(raw)
	return &value
}

// dryKey carries a --dry-run's dryDoc (overlay, argv) to askPrepareAndRun, which then stops before Prepare.
type dryKey struct{}

// dryDoc is what --dry-run prints.
type dryDoc struct {
	Agent     string              `json:"agent"`
	Model     string              `json:"model"`
	Route     string              `json:"route"`
	Hops      []string            `json:"hops"`
	Overlay   agentconfig.Overlay `json:"overlay"`
	Argv      string              `json:"argv"`
	Preflight []string            `json:"preflight,omitempty"`
	Skipped   []string            `json:"skipped,omitempty"` // targets the chain dropped before its first hop
	Terms     []contract.Term     `json:"terms,omitempty"`
	Plan      dryPlan             `json:"plan"`
}

// dryPlan is the route plan with its fallback policy spelled out; the shallower field wins the JSON name.
type dryPlan struct {
	agent.RoutePlan
	Fallback dryFallback `json:"fallback"`
}

type dryFallback struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
}

func policyKind(k router.PolicyKind) string {
	switch k {
	case router.PolicyNone:
		return "none"
	case router.PolicySameWorkspace:
		return "same-workspace"
	}
	return "any-in-fence"
}

func policySource(s router.PolicySource) string {
	return [...]string{router.PolicyDefault: "default", router.PolicyEnv: "env", router.PolicyOverlay: "overlay", router.PolicyTyped: "typed"}[s]
}

// dryRun fills base (overlay, argv) with what the run would do; it stops before any record, claim, spawn or model call.
func dryRun(ctx context.Context, req agent.RunOptions, base dryDoc, format string, out io.Writer) int {
	o, _, ch, err := newClient().Plan(agent.PeekContext(ctx), req)
	if err != nil {
		return failAskCode(format, req.Agent, err, prepareExitCode(err))
	}
	doc := base
	doc.Agent, doc.Model, doc.Hops = o.Agent, o.Model, ch.Hops()
	doc.Skipped = ch.Skipped
	if ch.Plan != nil {
		plan := *ch.Plan
		doc.Plan = dryPlan{RoutePlan: plan, Fallback: dryFallback{Kind: policyKind(plan.Fallback.Kind), Source: policySource(plan.Fallback.Source)}}
	}
	r, ok, err := dryRoute(ctx, o, req, ch)
	if err != nil {
		return failAskCode(format, req.Agent, err, prepareExitCode(err))
	}
	if ok {
		doc.Model, doc.Route = cmp.Or(o.Model, r.Model), r.Line()
		doc.Hops = firstHopOnRoute(ch, r)
	}
	if ch.Preflight != nil {
		doc.Preflight = ch.Preflight.Lines()
	}
	return doc.print(format, out)
}

// firstHopOnRoute is ch's hops with hop 0's model, and the own hop's when still auto, set to the peeked route's.
func firstHopOnRoute(ch agent.Chain, r agent.ModelRoute) []string {
	if len(ch.Dispatches) == 0 || r.Model == "" {
		return ch.Hops()
	}
	ch.Dispatches = slices.Clone(ch.Dispatches)
	for _, i := range []int{0, ch.OwnHop()} {
		if i < 0 || i > 0 && resolvedModel(ch.Dispatches[i].Model) {
			continue // an own hop with a served model id runs that one
		}
		if !strings.Contains(ch.Dispatches[i].Model, r.Model) && ch.Dispatches[i].Account == r.Account {
			ch.Dispatches[i].Model = r.Model
		}
	}
	return ch.Hops()
}

// resolvedModel reports m as a concrete id, not empty, auto or a family alias.
func resolvedModel(m string) bool {
	return m != "" && !agent.IsAuto(m) && !slices.Contains(models.AliasNames, m) && !providers.IsFamily(m)
}

// dryRoute is the first hop's route, from the calls `gateway resolve` makes; ok is false when it cannot resolve.
func dryRoute(ctx context.Context, o, req agent.RunOptions, ch agent.Chain) (agent.ModelRoute, bool, error) {
	ctx = agent.PeekContext(ctx)
	reg, err := providers.Load()
	if err != nil {
		return agent.ModelRoute{}, false, err
	}
	// o's model and profile cover a role rotation's pick, which the caller's req never names
	q := agent.QueryFor(ctx, agent.ModelQuery{Agent: o.Agent, Model: cmp.Or(req.Model, o.Model), Profile: cmp.Or(req.Provider, req.Lead, o.Provider, o.Lead), Account: req.Account,
		Effort: req.Effort, Fallback: o.Fallback, Task: o.Prompt, Tools: o.Tools,
		Held: req.Background || req.SessionID != "" || req.Host != ""})
	q = queryOnFirstHop(reg, q, ch)
	hop, note, err := agent.RunQuery(ctx, reg, q, agent.HopAlias(o.Prompt, router.RuleClassifier{}.Alias))
	if err != nil {
		return agent.ModelRoute{}, false, err
	}
	r, _, err := agent.ExplainRun(reg, agent.QueryFor(ctx, hop), time.Now())
	if err != nil {
		return agent.ModelRoute{}, false, err
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, true, nil
}

func joinTerms(ts []contract.Term) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = string(t)
	}
	return strings.Join(s, ", ")
}

func (d dryDoc) print(format string, out io.Writer) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(out, d); err != nil {
			return failAsk(format, d.Agent, err)
		}
		return 0
	}
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(out, "%-12s %s\n", k+":", v)
		}
	}
	row("agent", d.Agent)
	row("model", d.Model)
	row("route", d.Route)
	row("hops", strings.Join(d.Hops, "; "))
	row("terms", joinTerms(d.Terms))
	row("skipped", strings.Join(d.Skipped, "; "))
	if !d.Overlay.Empty() {
		b, _ := json.Marshal(d.Overlay)
		row("overlay", string(b))
	}
	row("argv", d.Argv)
	for _, l := range d.Preflight {
		row("preflight", l)
	}
	return 0
}

// queryOnFirstHop explains the admitted account/model rather than independently choosing the agent's pin again.
func queryOnFirstHop(reg *providers.Registry, q agent.ModelQuery, ch agent.Chain) agent.ModelQuery {
	if len(ch.Dispatches) == 0 {
		return q
	}
	d := ch.Dispatches[0]
	for _, name := range reg.Names() {
		if reg.Row(name).Account() == d.Account {
			q.Profile, q.Account = name, ""
			break
		}
	}
	return q
}
