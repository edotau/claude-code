package ask

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/dev/contract"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

// askKeys is each config flag and the overlay key it is a typed spelling of (D8); the other flags are not config.
var askKeys = map[string]string{
	"agent": "agent", "agents": "agents", "fallback": "fallback", "native": "native", "profile": "profile",
	"provider": "provider", "as": "identity", "model": "model", "tier": "tier", "effort": "effort", "tools": "tools",
	"no-tools": "noTools", "no-persist": "noPersist", "debug-file": "debugFile", "keep-env": "keep", "env": "set",
	"system-prompt": "systemPrompt", "append-system-prompt": "appendSystemPrompt", "output-schema": "outputSchema",
	"direct": "endpoint", "autonomous": "autonomous", "fleet": "fleet", "enable-memory": "enableMemory",
	"codemode": "codemode", "runner": "runner", "google-tools": "googleTools", "timeout": "timeout", "host": "host",
}

// endpointDirect is the endpoint key's spelling of --direct: the profile's own address, no proxy.
const endpointDirect = "direct"

// askChoices are the value sets the validators own; a flag outside them takes any value.
func askChoices() map[string][]string {
	return map[string][]string{"fallback": route.FallbackModes, "format": agent.Formats, "effort": agentid.EffortNames(),
		"as": {"auto", "user", "sp"}, "codemode": route.Codemodes, "runner": route.Runners, "tier": agent.TierNames()}
}

// keyKinds are the overlay keys whose wire type is not their flag's: endpoint is a name, set an object.
var keyKinds = map[string]string{"endpoint": "string", "set": "object"}

// askSchema is the overlay key list: each config flag under its overlay key, typed as the overlay takes it.
var askSchema = sync.OnceValue(func() Schema {
	fs, _ := askFlagSet()
	s := fieldsOf(fs, askKeys, askChoices())
	for i, f := range s.Fields {
		if k, ok := keyKinds[f.Name]; ok {
			s.Fields[i].Kind = k
		}
	}
	return s
})

// allFlagSchema is every ask flag under its own name, used for tab completion choice lists.
func allFlagSchema() Schema {
	fs, _ := askFlagSet()
	return fieldsOf(fs, nil, askChoices())
}

// briefParts reads --brief: the task keys and prompt only; configuration goes in --overlay.
func briefParts(value string, stdin io.Reader) (string, Task, error) {
	raw, err := agentconfig.ReadJSONSource("brief", value, stdin)
	if err != nil {
		return "", Task{}, err
	}
	prompt, task, err := parseBrief(raw)
	if err != nil {
		return "", Task{}, fmt.Errorf("brief: %w", err)
	}
	return prompt, task, nil
}

// parseBrief decodes one brief document: its prompt and task keys; any other key is refused by name.
func parseBrief(raw []byte) (string, Task, error) {
	doc, err := decodeDoc(raw)
	if err != nil {
		return "", Task{}, err
	}
	var prompt string
	if v, ok := doc[promptKey]; ok && (bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &prompt) != nil) {
		return "", Task{}, fmt.Errorf("brief %s: want a string", promptKey)
	}
	if err := checkKeys(doc, append([]string{promptKey}, taskKeys...)); err != nil {
		return "", Task{}, err
	}
	task, err := taskOf(doc)
	return prompt, task, err
}

// Field is one ask flag as an overlay key; Flag is set when the key's name is not the flag's.
type Field struct {
	Name    string
	Flag    string
	Kind    string // bool|int|duration|string|object
	Choices []string
}

// Schema is ask's overlay keys in FlagSet (sorted) order.
type Schema struct{ Fields []Field }

// FlagName is the flag f's key is typed as.
func (f Field) FlagName() string { return cmp.Or(f.Flag, f.Name) }

// Lookup returns the field named name.
func (s Schema) Lookup(name string) (Field, bool) {
	i := slices.IndexFunc(s.Fields, func(f Field) bool { return f.Name == name })
	if i < 0 {
		return Field{}, false
	}
	return s.Fields[i], true
}

// fieldsOf builds the schema from fs, skipping internal flags and --brief; a non-nil keys keeps only the config flags.
func fieldsOf(fs *flag.FlagSet, keys map[string]string, choices map[string][]string) Schema {
	var s Schema
	fs.VisitAll(func(f *flag.Flag) {
		key, isKey := keys[f.Name]
		if strings.HasPrefix(f.Usage, "internal:") || f.Name == briefFlagName || keys != nil && !isKey {
			return
		}
		fd := Field{Name: f.Name, Kind: kindOf(f.Value), Choices: choices[f.Name]}
		if isKey && key != f.Name {
			fd.Name, fd.Flag = key, f.Name
		}
		s.Fields = append(s.Fields, fd)
	})
	return s
}

func kindOf(v flag.Value) string {
	if b, ok := v.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	if g, ok := v.(flag.Getter); ok {
		switch g.Get().(type) {
		case int:
			return "int"
		case time.Duration:
			return "duration"
		}
	}
	return "string"
}

// briefFlagName is the one flag whose value is a whole document of keys, so never a key itself.
const briefFlagName = "brief"

// promptKey is the brief key for the task text.
const promptKey = "prompt"

// Task is a brief's execution contract: keys with no flag, rendered into the brief and run by the harness.
type Task struct {
	Own      []string
	Report   string
	Verify   string
	Contract *Contract
	// Action is a typed no-model step (one of actions) the harness runs in a plain child; it is not a brief key.
	Action string
}

// actions are the values of the brief's action key: each runs one existing verb with Go-built argv and no model.
var actions = []string{"gate", "diff", "gateway", "scope"}

// Contract is the brief's contract key: the unit, its size and optional limits; Go derives every other field.
type Contract struct {
	Unit   string         `json:"unit"`
	Kind   string         `json:"kind"`
	Limits ContractLimits `json:"limits"`
	// Role is "owner" (default) or "fleet"; a fleet member names the owner contract it joins as Parent.
	Role   string `json:"role"`
	Parent string `json:"parent"`
}

// ContractLimits overrides the kind's default budget; zero keeps the default.
type ContractLimits struct {
	Tokens int `json:"tokens"`
	Leaves int `json:"leaves"`
}

// Set reports whether any task key was given.
func (t Task) Set() bool {
	return t.Own != nil || t.Report != "" || t.Verify != "" || t.Contract != nil
}

// taskKeys are the brief keys that carry the Task rather than argv.
var taskKeys = []string{"own", "report", "verify", "contract", "action"}

func decodeDoc(raw []byte) (map[string]json.RawMessage, error) {
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("brief: not a JSON object: %w", err)
	}
	if doc == nil {
		return nil, errors.New("brief: not a JSON object")
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return nil, errors.New("brief: trailing data after the JSON object")
	}
	return doc, nil
}

func taskOf(doc map[string]json.RawMessage) (t Task, err error) {
	for _, k := range taskKeys {
		v, ok := doc[k]
		if !ok {
			continue
		}
		if k == "contract" {
			if t.Contract, err = contractOf(v); err != nil {
				return Task{}, err
			}
			continue
		}
		dst := cmp.Or(map[string]any{"own": &t.Own, "verify": &t.Verify, "action": &t.Action}[k], any(&t.Report))
		want := map[bool]string{true: "an array of strings", false: "a string"}[k == "own"]
		if json.Unmarshal(v, dst) != nil || string(v) == "null" || string(v) == `""` {
			return Task{}, fmt.Errorf("brief %s: want %s", k, want)
		}
	}
	return t, t.checkAction()
}

func (t Task) checkAction() error {
	if t.Action == "" || slices.Contains(actions, t.Action) {
		return nil
	}
	return fmt.Errorf("brief action %q unknown (%s)", t.Action, strings.Join(actions, "|"))
}

// contractOf decodes the contract key strictly: an unknown field is a typo, never ignored.
func contractOf(v json.RawMessage) (*Contract, error) {
	const want = "brief contract: want {\"unit\": \"...\", \"kind\": \"...\", \"limits\": {\"tokens\": N, \"leaves\": N}}"
	if string(v) == "null" {
		return nil, errors.New(want)
	}
	var c Contract
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %v", want, err)
	}
	if (c.Role != "" && c.Role != "owner" && c.Role != "fleet") || (c.Role == "fleet") != (c.Parent != "") {
		return nil, errors.New("brief contract: role is owner or fleet, and parent goes with role fleet only")
	}
	return &c, nil
}

// checkKeys refuses the first key (sorted) not in valid, naming the nearest valid key.
func checkKeys(doc map[string]json.RawMessage, valid []string) error {
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if slices.Contains(valid, k) {
			continue
		}
		if n, ok := textutil.Nearest(k, valid); ok {
			return fmt.Errorf("brief key %q unknown: did you mean %q?", k, n)
		}
		return fmt.Errorf("brief key %q unknown (keys: %s)", k, strings.Join(valid, ", "))
	}
	return nil
}

// runKeyFields are o's per-run keys by the flag that types each: the route unit and env layer are not among them.
func runKeyFields(o *agentconfig.Overlay) (map[string]*string, map[string]**bool) {
	return map[string]*string{"agent": &o.Agent, "model": &o.Model, "effort": &o.Effort, "fallback": &o.Fallback,
			"runner": &o.Runner, "host": &o.Host, "codemode": &o.Codemode, "tier": &o.Tier, "debug-file": &o.DebugFile,
			"system-prompt": &o.SystemPrompt, "append-system-prompt": &o.AppendSystemPrompt,
			"output-schema": &o.OutputSchema, "google-tools": &o.GoogleTools, "timeout": &o.Timeout},
		map[string]**bool{"tools": &o.Tools, "no-tools": &o.NoTools, "no-persist": &o.NoPersist, "native": &o.Native,
			"autonomous": &o.Autonomous, "fleet": &o.Fleet, "enable-memory": &o.EnableMemory}
}

// overlayFlags is ov's per-run keys as the flag values that type them; endpoint direct is --direct.
func overlayFlags(ov agentconfig.Overlay) map[string]string {
	out := map[string]string{}
	strs, bools := runKeyFields(&ov)
	for name, p := range strs {
		if *p != "" {
			out[name] = *p
		}
	}
	for name, p := range bools {
		if *p != nil {
			out[name] = strconv.FormatBool(**p)
		}
	}
	if len(ov.Agents) > 0 {
		out["agents"] = string(ov.Agents)
	}
	if ov.Endpoint == endpointDirect {
		out["direct"] = "true"
	}
	return out
}

// typeOverlay fs.Sets ov's per-run keys (untyped timeout stays untyped); a flag wins unless in retype.
func typeOverlay(fs *flag.FlagSet, ov agentconfig.Overlay, retype []string, typed bool) []string {
	var set []string
	for name, v := range overlayFlags(ov) {
		if helptext.Explicit(fs, name) && !slices.Contains(retype, name) {
			continue
		}
		if name == "timeout" && !typed { // an inherited deadline leaves slow-model handling and cutoff records on
			_ = fs.Lookup(name).Value.Set(v) // not fs.Set: the flag stays untyped
			continue
		}
		_ = fs.Set(name, v) // the overlay's own decode checked v against the same rule the flag holds
		set = append(set, name)
	}
	return set
}

// flagsOverlay is the per-run flags typed on fs as overlay keys; --agents counts only as inline JSON.
func flagsOverlay(fs *flag.FlagSet) agentconfig.Overlay {
	var o agentconfig.Overlay
	strs, bools := runKeyFields(&o)
	fs.Visit(func(fl *flag.Flag) {
		v := fl.Value.String()
		if p, ok := strs[fl.Name]; ok {
			*p = v
		}
		if p, ok := bools[fl.Name]; ok {
			*p = new(v == "true")
		}
		if fl.Name == "agents" && strings.HasPrefix(strings.TrimSpace(v), "{") {
			o.Agents = json.RawMessage(v)
		}
	})
	return settleTools(o)
}

// settleTools sets the other half of a typed tools pair, so a stored half ask never applied cannot contradict it.
func settleTools(o agentconfig.Overlay) agentconfig.Overlay {
	switch {
	case o.Tools != nil && *o.Tools && o.NoTools == nil:
		o.NoTools = new(false)
	case o.NoTools != nil && *o.NoTools && o.Tools == nil:
		o.Tools = new(false)
	}
	return o
}

// overlayTimeout is the key v unless d is outside its domain (positive, at most 24h); the flag takes wider ones.
func overlayTimeout(v string, d time.Duration) string {
	if d <= 0 || d > 24*time.Hour {
		return ""
	}
	return v
}

// mergeFlags lays the per-run flags typed here onto the overlay (HopAppend), after their own checks ran.
func (r *askRun) mergeFlags() (int, bool) {
	fo, err := r.flagUnit(flagsOverlay(r.fs))
	fo.Timeout = overlayTimeout(fo.Timeout, r.f.timeout)
	if err != nil {
		return r.fail(err, 2)
	}
	base := r.ov
	if slices.Contains(r.f.cli, "timeout") && fo.Timeout == "" {
		base.Timeout = "" // a typed --timeout outside the key's domain drops the overlay's own deadline
	}
	ov, err := base.Derive(fo, agentconfig.HopAppend)
	if err != nil {
		return r.fail(fmt.Errorf("overlay: %w", err), 2)
	}
	r.ov = r.directEndpoint(ov)
	return 0, false
}

// directEndpoint sets or clears ov's endpoint direct as --direct says: it is the unit's endpoint, and false wins.
func (r *askRun) directEndpoint(ov agentconfig.Overlay) agentconfig.Overlay {
	switch {
	case r.f.direct && ov.Provider != "" && ov.Profile != "":
		ov.Endpoint = endpointDirect
	case !r.f.direct && slices.Contains(r.f.cli, "direct") && ov.Endpoint == endpointDirect:
		ov.Endpoint = ""
	}
	return ov
}

// flagUnit makes the --provider/--profile route fo's unit and clears the flags: it runs as an overlay unit (owner).
func (r *askRun) flagUnit(fo agentconfig.Overlay) (agentconfig.Overlay, error) {
	f := r.f
	fr := flagRoute(f)
	if fr.Profile == "" || cmp.Or(f.host, f.job, f.retry, f.batchSrc) != "" {
		return fo, nil // none, the session lead, a provider alone without a row, or a run the record or far side routes
	}
	if f.provider != "" && f.provider != fr.Provider {
		return fo, fmt.Errorf("--profile %s is a %s profile, not %s: %s", f.profile, fr.Provider, f.provider, routeForms)
	}
	if err := flagFence(r.ctx, f.profile, f.provider, false); err != nil { // name the flag before it becomes the unit
		return fo, err
	}
	u := agent.UnitOf(r.ov, fr.Provider, fr.Profile) // the overlay's endpoint, identity and account on the same provider
	fo.Endpoint, fo.Provider, fo.Profile, fo.Identity, fo.Account = u.Endpoint, u.Provider, u.Profile, u.Identity, u.Account
	f.profile, f.provider = "", ""
	return fo, nil
}

// withoutTyped is ov less each per-run key whose flag fs holds typed: what still needs an --overlay on a re-run's argv.
func withoutTyped(fs *flag.FlagSet, ov agentconfig.Overlay) agentconfig.Overlay {
	strs, bools := runKeyFields(&ov)
	for name, p := range strs {
		if helptext.Explicit(fs, name) {
			*p = ""
		}
	}
	for name, p := range bools {
		if helptext.Explicit(fs, name) {
			*p = nil
		}
	}
	if helptext.Explicit(fs, "agents") {
		ov.Agents = nil
	}
	if ov.Endpoint == endpointDirect && helptext.Explicit(fs, "direct") {
		ov.Endpoint = ""
	}
	return ov
}

// specBrief is prompt as the leaf brief the own, report and verify keys describe; no contract returns it as is.
func specBrief(prompt string, argv []string, t Task) (string, *contractOpen, error) {
	if !t.Set() {
		return prompt, nil, nil
	}
	if t.Verify != "" && plans.VerifyShape(t.Verify) != nil {
		return "", nil, errors.New("brief verify: want go build|vet|test on ./ packages, agents diff --check or doctor, " +
			"rtk forms, joined by &&")
	}
	if strings.ContainsAny(t.Report, "\r\n") {
		return "", nil, errors.New("brief report: want a one-line path")
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
	typeOverlayArg(fs, f.overlay)
	if f.batchSrc != "" || f.loop || f.live || f.waves != "" || f.retry != "" || f.job != "" {
		return briefFlags{}, errors.New("brief own, report, verify and contract are for one new dispatch: not with --batch, --loop, " +
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

// typeOverlayArg types a readable --overlay's per-run keys (agent, tier, code mode) for the brief; the real parse errs.
func typeOverlayArg(fs *flag.FlagSet, value string) {
	if value == "" || value == "-" {
		return
	}
	if ov, err := agentconfig.LoadOverlayFrom(context.Background(), value, nil, true); err == nil {
		typeOverlay(fs, ov, nil, true)
	}
}

// takeAction stores the spec's no-model action on f; it refuses any task text.
func takeAction(f *askFlags, task Task, prompt string, pos []string) error {
	if len(pos) > 0 || strings.TrimSpace(prompt) != "" || task.Report != "" || task.Verify != "" || task.Contract != nil {
		return errors.New("brief action runs no model: drop the prompt, task words, report, verify and contract (own and agent/model stay)")
	}
	f.action, f.actionOwn = task.Action, task.Own
	return nil
}

// withBrief expands --brief into the prompt/action on f and drops --brief from args.
func withBrief(f *askFlags, args, pos []string) ([]string, error) {
	prompt, task, err := briefParts(f.brief, os.Stdin)
	if err != nil {
		return nil, err
	}
	out := dropBrief(args)
	if task.Action != "" {
		return out, takeAction(f, task, prompt, pos)
	}
	return withPrompt(f, out, args, pos, prompt, task)
}

// withPrompt appends the brief's rendered prompt to out unless a positional task was typed.
func withPrompt(f *askFlags, out, args, pos []string, prompt string, task Task) ([]string, error) {
	target, err := briefArgv(f.overlay, out, pos, prompt, task)
	if err != nil {
		return nil, err
	}
	if prompt, f.contract, err = specBrief(prompt, target, task); err != nil {
		return nil, err
	}
	f.verify = task.Verify
	if prompt != "" && len(pos) == 0 {
		if _, term := walkArgs(args, []string{briefFlagName}); term < 0 {
			out = append(out, "--")
		}
		out = append(out, prompt)
	}
	return out, nil
}

// briefArgv validates task keys against positional prompt and stdin overlay, returning args for briefTarget.
func briefArgv(overlay string, out, pos []string, prompt string, task Task) ([]string, error) {
	switch {
	case task.Set() && (len(pos) > 0 || strings.TrimSpace(prompt) == "" || prompt == "-"):
		return nil, errors.New("brief own, report, verify and contract wrap the brief's own inline prompt: set prompt, no positional task")
	case task.Set() && overlay == "-": // the brief names its agent and tier before stdin could be read
		return nil, errors.New("own, report, verify and contract need the --overlay inline or in a file, not -")
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

// parseAsk parses args, re-parses them with --brief's prompt when given, then refuses a leading agent word.
func parseAsk(args []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	fs, f := askFlagSet()
	pos, code, done := parseAskArgs(fs, args)
	f.cli = typedFlags(fs)
	raw := args
	if !done && f.brief != "" {
		fs, f, args, pos, code, done = reparseBrief(f, args, pos)
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

// reparseBrief re-parses args with the brief's prompt appended on a fresh FlagSet; f.brief survives for stdinTaken.
func reparseBrief(f *askFlags, args, pos []string) (*flag.FlagSet, *askFlags, []string, []string, int, bool) {
	merged, err := withBrief(f, args, pos)
	if err != nil {
		return nil, f, args, nil, failAskCode(f.format, f.name, err, 2), true
	}
	fs, nf := askFlagSet()
	pos, code, done := parseAskArgs(fs, merged)
	nf.brief, nf.verify, nf.contract, nf.cli = f.brief, f.verify, f.contract, f.cli
	nf.action, nf.actionOwn = f.action, f.actionOwn
	return fs, nf, merged, pos, code, done
}

// typedFlags are the flags typed on fs's own command line: before any spec, overlay or record sets another.
func typedFlags(fs *flag.FlagSet) []string {
	var names []string
	fs.Visit(func(fl *flag.Flag) { names = append(names, fl.Name) })
	return names
}

// refuseAgentWord rejects a first task word that is exactly an agent name when --agent was not given; "--" opts out.
func refuseAgentWord(fs *flag.FlagSet, args, pos []string) error {
	if w, ok := leadingAgentWord(args, pos, agent.RosterNames(), helptext.Explicit(fs, "agent")); ok {
		return fmt.Errorf("%q is an agent name; use --agent %s (or -- before a prompt that starts with it)", w, w)
	}
	return nil
}

// leadingAgentWord is pos[0] when it is a roster agent name, --agent is untyped and no "--" precedes it.
func leadingAgentWord(args, pos, roster []string, agentTyped bool) (string, bool) {
	if agentTyped || len(pos) == 0 || !slices.Contains(roster, pos[0]) {
		return "", false
	}
	if i := slices.Index(args, "--"); i >= 0 && len(pos) <= len(args)-i-1 {
		return "", false
	}
	return pos[0], true
}

// walkArgs finds each word of the names flags (and its value) and the real "--" terminator, skipping other values.
func walkArgs(args, names []string) (drop map[int]bool, term int) {
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
		if slices.Contains(names, name) {
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

// dropBrief removes --brief V and --brief=V so the re-parse does not read the document twice.
func dropBrief(args []string) []string {
	drop, _ := walkArgs(args, []string{briefFlagName})
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

func canonicalArgvSkip(name string) bool {
	return name == briefFlagName || name == "dry-run"
}

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

// retypeRoute types the --profile/--provider this line typed that folding or resolveLead moved off the flags.
func (r *askRun) retypeRoute() {
	fs, typed := askFlagSet()
	fs.SetOutput(io.Discard)
	_, _ = helptext.ParseArgs(fs, r.args)
	if r.f.profile == "" && typed.profile != "" {
		_ = r.fs.Set("profile", typed.profile)
	}
	if r.f.provider == "" && typed.provider != "" {
		_ = r.fs.Set("provider", typed.provider)
	}
}

// dryCtx is ctx carrying the dry doc (overlay, argv) when --dry-run is set.
func (r *askRun) dryCtx(ctx context.Context, prompt string) context.Context {
	if !r.f.dryRun {
		return ctx
	}
	r.retypeRoute()
	if fr := flagRoute(r.f); r.f.host == "" && r.f.provider == "" && r.f.profile != "" && fr.Provider != "" { // the re-run types the pair
		_ = r.fs.Set("provider", fr.Provider)
	}
	overlayArg := r.canonicalOverlayArg()
	argv := canonicalArgv(r.fs, r.f.listValues(), prompt, taskFromStdin(r.pos), overlayArg)
	ov := r.ov
	ov.Schema = 0 // a flag line and its overlay twin preview the same document
	return context.WithValue(ctx, dryKey{}, dryDoc{Overlay: ov, Argv: textutil.ShellJoin(argv), Terms: r.f.contract.terms()})
}

// canonicalOverlayArg is the --overlay a re-run types: the typed document less the keys now typed as flags.
func (r *askRun) canonicalOverlayArg() *string {
	if !r.ovTyped {
		return nil
	}
	ov := withoutTyped(r.fs, r.typedOv)
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
