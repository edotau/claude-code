package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Schema is one agent definition: a roster CLI, or a prompt-bearing definition that runs on one.
type Schema struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"` // nil = inherit, empty = no tools
	Model       string   `json:"model,omitempty"`
	MaxTurns    int      `json:"max_turns,omitempty"`
	Agent       string   `json:"runner,omitempty"`  // roster agent that runs it; "" = claude
	Profile     string   `json:"profile,omitempty"` // provider profile; "" = routed
	Effort      string   `json:"effort,omitempty"`
	CLI         CLI      `json:"-"` // roster facts; zero on a definition
}

// CLI is a roster row's platform group.
type CLI struct {
	Binary string // executable on PATH; "" = claude, which the harness launches itself (not a roster agent)
	// Dialects are the wires the agent speaks, preferred first; none = it runs only on its own login.
	Dialects []string
	// RouterSuffix is the path appended to the router session base for a wire; it covers every Dialects entry.
	RouterSuffix map[string]string
	// MinVersion and MaxVersion bound the CLI versions the harness is known to work with; "" = no bound.
	// MaxVersion may end in ".x" ("1.x" = any 1.y.z). Each bound carries its one-line reason.
	MinVersion, MinReason string
	MaxVersion, MaxReason string
	Caps                  Caps
	Events                []string        // typed event kinds the stream can carry
	Cost                  bool            // reports a cost
	ResumeFormat          string          // "codex resume %s"; "" = none
	ModelsArgv            []string        // args that list models; nil = no catalog verb
	ModelsProvider        string          // list via models --live on this provider instead
	ReadNeedsTools        bool            // file reads are denied without --tools
	PromptVia             PromptTransport // how a headless prompt reaches the agent; only PromptArgv is size-capped
	PromptMaxBytes        int             // a verified per-agent prompt ceiling below the transport's; 0 = none
	PackVia               PackTransport   // how packed workspace context reaches the agent; zero = inline in the prompt
	DirFlag               bool            // Headless.WorkDir must be set; cmd.Dir alone is not enough
	Hub                   bool            // ask routes it through the opencode hub unless --native is set
	ConfigureHint         string          // the command to run when no credential resolved
	DisplayName           string          // TUI label; "" = Name
	LoginArgv             []string        // the CLI's own sign-in command
	ExtraUsage            [][2]string     // extra help rows
	CatalogUsage          string          // override for the catalog help row
	CatalogHint           string          // extra hint on the catalog page
	OwnLoginProfile       string          // the pin naming the agent's own sign-in when no registry row does; "" = none
	PinIDs                []string        // extra ids accepted as this agent's profile pin besides registry rows

	// Extras maps the record extras keys the CLI reads to their allowed values; nil = not declared yet, not validated.
	Extras map[string][]string
}

// PromptTransport is how a headless prompt reaches an agent; the zero value is the capped argv (fail closed).
type PromptTransport uint8

const (
	PromptArgv    PromptTransport = iota // one argv element: MaxPromptBytes applies
	PromptStdin                          // the child's stdin
	PromptMessage                        // an SDK or API message body
)

func (t PromptTransport) String() string {
	return [...]string{"argv", "stdin", "message"}[min(t, PromptMessage)]
}

// PackTransport is how packed workspace context reaches an agent; the zero value inlines it in the prompt.
type PackTransport uint8

const (
	PackInline PackTransport = iota // the packed text is the prompt
	PackFile                        // the pack is a file in the workdir the agent reads with its tools
)

// MaxBytes is the largest prompt t carries; 0 means no transport cap (the model window still applies).
func (t PromptTransport) MaxBytes() int {
	if t == PromptArgv {
		return MaxPromptBytes
	}
	return 0
}

// Agent is everything one vendor package contributes at init: its roster row, driver and optional extras.
type Agent struct {
	Row       Schema
	Driver    Driver
	Launcher  Launcher                       // the default Claude launcher; at most one agent sets it
	Hub       *Hub                           // the opencode-routed dispatch ladder; at most one agent sets it
	Redactors map[string]func(string) string // launch env key -> redactor of its structured value
}

var (
	registryMu sync.RWMutex
	rosterRows []Schema // sorted by name; filled by Register from each agent's init
	drivers    = map[string]Driver{}
)

// Register adds an agent from its init; a wiring bug (nil or mismatched Driver, duplicate name, second Launcher or Hub) panics.
func Register(a Agent) {
	registryMu.Lock()
	defer registryMu.Unlock()
	switch {
	case a.Driver == nil:
		panic("agent: Register " + a.Row.Name + " with a nil Driver")
	case a.Row.Name != a.Driver.Name():
		panic("agent: row " + a.Row.Name + " registered with driver " + a.Driver.Name())
	case !agentid.Known(a.Row.Name):
		panic("agent: Register " + a.Row.Name + ", not a roster agent")
	case a.Launcher != nil && defaultLauncher != nil:
		panic("agent: second Launcher registered by " + a.Row.Name)
	case a.Hub != nil && hub != nil:
		panic("agent: second Hub registered by " + a.Row.Name)
	}
	addRow(a.Row)
	drivers[a.Row.Name] = a.Driver
	if a.Launcher != nil {
		defaultLauncher = a.Launcher
	}
	if a.Hub != nil {
		hub = a.Hub
	}
	maps.Copy(envRedactors, a.Redactors)
}

// RegisterRowForTest adds a roster row with no driver, for tests that cannot import the vendor packages.
func RegisterRowForTest(row Schema) {
	registryMu.Lock()
	defer registryMu.Unlock()
	addRow(row)
}

// addRow inserts row in name order; the caller holds registryMu.
func addRow(row Schema) {
	i, dup := slices.BinarySearchFunc(rosterRows, row.Name, func(r Schema, n string) int { return strings.Compare(r.Name, n) })
	if dup {
		panic("agent: duplicate agent " + row.Name)
	}
	rosterRows = slices.Insert(rosterRows, i, row)
}

// driverFor is the registered Driver for name.
func driverFor(name string) (Driver, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := drivers[name]
	return d, ok
}

// mustDriver is the registered Driver for name; a name with none is a wiring bug.
func mustDriver(name string) Driver {
	d, ok := driverFor(name)
	if !ok {
		panic("agent: no driver registered for " + name)
	}
	return d
}

// rows is the registered roster rows in order, shared and not to be mutated; Roster returns copies.
func rows() []Schema {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return slices.Clone(rosterRows)
}

// RosterNames is every roster agent's name, in roster order (by name).
func RosterNames() []string {
	rs := rows()
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// Roster returns independent copies of the roster rows, with external wire facts filled in.
func Roster() []Schema {
	rs := rows()
	for i := range rs {
		rs[i] = cloneSchema(rs[i])
	}
	return rs
}

// cloneSchema returns s with its slice and map fields copied, so a caller cannot mutate the registry.
func cloneSchema(s Schema) Schema {
	s.Tools = slices.Clone(s.Tools)
	s.CLI.Dialects = slices.Clone(s.CLI.Dialects)
	s.CLI.RouterSuffix = maps.Clone(s.CLI.RouterSuffix)
	s.CLI.Events = slices.Clone(s.CLI.Events)
	s.CLI.ModelsArgv = slices.Clone(s.CLI.ModelsArgv)
	s.CLI.PinIDs = slices.Clone(s.CLI.PinIDs)
	s.CLI.LoginArgv = slices.Clone(s.CLI.LoginArgv)
	s.CLI.ExtraUsage = slices.Clone(s.CLI.ExtraUsage)
	return s
}

// LookupSchema returns a roster row by name, claude included.
func LookupSchema(name string) (Schema, bool) {
	s, ok := lookupRow(name)
	return cloneSchema(s), ok
}

// lookupRow is the shared registry row for name, not to be mutated; readers that do not keep it skip the clone.
func lookupRow(name string) (Schema, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	i, ok := slices.BinarySearchFunc(rosterRows, name, func(r Schema, n string) int { return strings.Compare(r.Name, n) })
	if !ok {
		return Schema{}, false
	}
	return rosterRows[i], true
}

// External reports whether the row drives an external CLI rather than the harness's Claude launcher.
func (s Schema) External() bool { return s.CLI.Binary != "" }

// ResumeHint renders the CLI's resume command, empty when the format or session id is empty.
func (s Schema) ResumeHint(id string) string {
	if s.CLI.ResumeFormat == "" || id == "" {
		return ""
	}
	return fmt.Sprintf(s.CLI.ResumeFormat, id)
}

func errUnknownAgent(name string) error { return fmt.Errorf("unknown agent %q", name) }

// ModelsProfile is the agent's profile on its ModelsProvider that carries the agent's own login.
func (s Schema) ModelsProfile(reg *providers.Registry) (string, error) {
	if p, ok := reg.LoginProfile(s.CLI.ModelsProvider, s.Name); ok {
		return p.Name, nil
	}
	return "", fmt.Errorf("%s: no %s profile with a %s login", s.Name, s.CLI.ModelsProvider, s.Name)
}

// GeminiGatewayRow is a keyed Databricks row agy can reach through the router's native Gemini route.
func GeminiGatewayRow(p *providers.Provider) bool {
	return p != nil && p.Preset == providers.PresetDatabricks && p.Auth.Login == "" &&
		p.Auth.Method() != providers.AuthNone && p.Auth.Method() != providers.AuthCLI
}

// CanServe reports whether p is usable by agent name (Serves without the reason).
func CanServe(name string, p *providers.Provider) bool {
	if name == agentid.Gemini && GeminiGatewayRow(p) {
		return true
	}
	ok, _ := Serves(name, p, "")
	return ok
}

// Serves reports whether p is usable by agent: a login provider naming it, or a route on one of the dialects it speaks.
// A non-empty model adds that the model's wire (WireFor) is one the agent speaks. why names the first thing missing.
func Serves(agent string, p *providers.Provider, model string) (ok bool, why string) {
	spec, known := lookupRow(agent)
	if !known || !spec.External() {
		return false, fmt.Sprintf("unknown agent %q", agent)
	}
	// A cli-auth provider carries a login only its own agent can forward; a dialect match is not enough.
	if p.Auth.Login != "" {
		if p.Auth.Login == agent {
			return true, ""
		}
		return false, fmt.Sprintf("provider %s carries a %s login, which %s cannot use", p.Name, p.Auth.Login, agent)
	}
	if len(spec.CLI.Dialects) == 0 {
		return false, fmt.Sprintf("%s has no custom base URL; it runs only on its Google sign-in, not provider %s", spec.CLI.Binary, p.Name)
	}
	if !slices.ContainsFunc(spec.CLI.Dialects, func(d string) bool { _, ok := p.Route(d); return ok }) {
		return false, fmt.Sprintf("provider %s serves no %s route (it has %s)", p.Name,
			strings.Join(spec.CLI.Dialects, "/"), strings.Join(p.Dialects(), ","))
	}
	if p.Auth.Method() == providers.AuthCLI {
		return false, fmt.Sprintf("provider %s uses cli (subscription) auth, which %s cannot use; pick a keyed provider", p.Name, agent)
	}
	return servesModel(spec, agent, p, model)
}

func servesModel(spec Schema, agent string, p *providers.Provider, model string) (bool, string) {
	if model == "" {
		return true, ""
	}
	wire, ok, why := providers.WireFor(p, model)
	if !ok {
		return false, why
	}
	_, routed := spec.CLI.RouterSuffix[wire]
	if !routed && !slices.Contains(spec.CLI.Dialects, wire) {
		return false, fmt.Sprintf("%s does not speak the %s wire that provider %s uses for %s", agent, wire, p.Name, model)
	}
	return true, ""
}

// googleHost is Gemini's own API host; a gemini-dialect route anywhere else is a gateway.
const googleHost = "generativelanguage.googleapis.com"

func IsGateway(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != googleHost
}

// Names lists the third-party agents in roster order.
func Names() []string {
	var out []string
	for _, s := range Roster() {
		if s.External() {
			out = append(out, s.Name)
		}
	}
	return out
}

// Headless is the non-interactive form of a run; zero Tools means tools are disabled where the CLI allows it.
type Headless struct {
	Prompt     string
	Effort     string
	SessionID  string // resume this session
	Tools      bool
	Autonomous bool     // opencode: --auto
	Attach     []string // opencode: one -f per path
	WorkDir    string   // opencode: --dir; cmd.Dir alone doesn't move opencode's own cwd resolution
	NoPersist  bool     // codex: --ephemeral
	SafeMode   bool     // codex: --ignore-user-config --ignore-rules; opencode: --pure
}

// ModelAsk is one agent-model decision, shared by the interactive launch and the headless dispatch.
type ModelAsk struct {
	Agent string
	// Flag is --model as typed: a model alias (opus) or a model id; "" = none.
	Flag string
	// Target is the provider and the model the agent runs with no flag and no pin.
	Target providers.Target
	// Applied means Target.Model already carries Flag (the session selection spelled it for Target.Provider).
	Applied bool
}

// agentModelEnv is each CLI agent's own model knob; claude's ANTHROPIC_MODEL is handled in Claude().
var agentModelEnv = func() map[string]string {
	m := map[string]string{}
	for _, a := range agentid.External() {
		m[a] = strings.ToUpper(a) + "_MODEL"
	}
	return m
}()

// AgentModelPin is the agent's own model: its shell model env (local config) over the vault record's model, or "".
func AgentModelPin(agent string) string {
	if v := strings.TrimSpace(os.Getenv(agentModelEnv[agent])); v != "" && agentModelEnv[agent] != "" {
		return v
	}
	return vault.GetAgentRecord(agent).Model
}

// ResolveAgentModel is the one rule for the model an agent runs: flag > the agent's shell model env > its vault pin > Target.Model,
// spelled for Target.Provider. A model alias the provider lacks keeps Target.Model and says so in the note.
func ResolveAgentModel(a ModelAsk) (providers.Target, string) {
	t, p := a.Target, a.Target.Provider
	if a.Flag == "" {
		return pinnedAgentModel(a.Agent, t), ""
	}
	if a.Applied {
		return t, ""
	}
	alias := slices.Contains(models.AliasNames, a.Flag)
	switch {
	case p == nil && !alias:
		t.Model = a.Flag
	case p == nil:
	case alias && p.Models[a.Flag] != "":
		t.Model = p.Models[a.Flag]
	case alias:
		return t, fmt.Sprintf("%s has no %s model; keeping %s", p.Name, a.Flag, t.Model)
	default: // preflight's spelling: a bare id the workspace serves runs as databricks-<id>
		t.Model = launchWire(a.Agent, p, a.Flag)
	}
	return t, ""
}

func pinnedAgentModel(agent string, t providers.Target) providers.Target {
	if pin := AgentModelPin(agent); pin != "" {
		if m := providers.SpellPin(t.Provider, pin); m != "" {
			t.Model = m
		}
	}
	return t
}

// VaultAgentNames is claude, then the other roster agents alphabetically, then extra overlay agent names.
func VaultAgentNames(vs map[string]any) []string {
	var names, rest []string
	for _, sp := range Roster() {
		if !sp.External() {
			names = append(names, sp.Name)
		} else {
			rest = append(rest, sp.Name)
		}
	}
	names = append(names, slices.Sorted(slices.Values(rest))...)
	for _, name := range slices.Sorted(maps.Keys(vs)) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// VaultAgentIsClaude reports the roster row the harness launches itself (no external binary).
func VaultAgentIsClaude(name string) bool {
	s, ok := lookupRow(name)
	return ok && !s.External()
}

// AgentOn and AgentOff are the values `<agent> on|off` and ValidAgentActive accept.
const (
	AgentOn  = "on"
	AgentOff = "off"
)

// ValidAgentActive rejects any value but on|off, and off for claude.
func ValidAgentActive(agent, v string) error {
	if _, ok := lookupRow(agent); !ok {
		return errUnknownAgent(agent)
	}
	switch v {
	case AgentOn:
		return nil
	case AgentOff:
		if VaultAgentIsClaude(agent) {
			return errors.New("claude is always on")
		}
		return nil
	}
	return fmt.Errorf("active must be %q or %q, got %q", AgentOn, AgentOff, v)
}

// AgentUnbillable is why copilot has no account to bill (no GitHub login under 95%, Databricks profile or key); "" else.
func AgentUnbillable(agent string) string {
	if agent != agentid.Copilot {
		return ""
	}
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	if SchemaFor(agentid.Copilot).Eng().DescribeRoute(reg).OK {
		return ""
	}
	return "no account to bill"
}

// AgentActive reports whether agent is on: claude always, else a profile is set (copilot's no-bill-off stays derived).
func AgentActive(agent string) bool {
	if agent == agentid.Claude {
		return true
	}
	if _, ok := lookupRow(agent); !ok {
		return true
	}
	return vault.GetAgentRecord(agent).Profile != ""
}

// AgentPins reads the records once and returns every agent's spec; claude's profile is the session's, else its record's.
func AgentPins() (map[string]router.PinSpec, error) {
	recs, err := vault.ReadAgentRecords(RosterNames())
	if err != nil {
		return nil, fmt.Errorf("agent pins: %w", err)
	}
	var regErr error
	reg := sync.OnceValue(func() *providers.Registry { r, err := providers.Load(); regErr = err; return r }) // only a pinned row reads it
	rs := rows()
	out := make(map[string]router.PinSpec, len(rs))
	for _, k := range rs {
		r := recs[k.Name]
		spec := router.PinSpec{Profile: r.Profile, Off: k.External() && r.Profile == ""}
		if ownLogin(reg, k.Name, r.Profile) {
			spec.Profile = "" // the agent's own login profile routes as the old unset pin did
		}
		spec.Model = r.Model
		if !k.External() {
			spec.Profile = cmp.Or(providers.SessionLead(nil, false), spec.Profile)
		}
		out[k.Name] = spec
	}
	if regErr != nil { // a broken registry would otherwise make every own-login pin read as a real profile pin
		return nil, fmt.Errorf("agent pins: %w", regErr)
	}
	return out, nil
}

// OwnLogin reports a profile that is agent's own sign-in, which AgentPins reads as no pin; a broken registry errors.
func OwnLogin(agent, profile string) (bool, error) {
	var regErr error
	own := ownLogin(func() *providers.Registry { r, err := providers.Load(); regErr = err; return r }, agent, profile)
	if regErr != nil {
		return false, fmt.Errorf("own login: %w", regErr)
	}
	return own, nil
}

// ownLogin reports a profile that is agent's own sign-in: its schema's OwnLoginProfile, else a row with Auth.Login == agent.
func ownLogin(loadReg func() *providers.Registry, agent, profile string) bool {
	if profile == "" {
		return false
	}
	if s, ok := lookupRow(agent); ok && s.CLI.OwnLoginProfile != "" {
		return profile == s.CLI.OwnLoginProfile
	}
	reg := loadReg()
	if reg == nil {
		return false
	}
	p := reg.Providers[profile]
	return p != nil && p.Auth.Login == agent
}

// ValidAgentPin checks v as agent's profile pin: a registry profile, or its schema's own-login id or extra pin ids.
func ValidAgentPin(reg *providers.Registry, agent, v string) error {
	s, ok := lookupRow(agent)
	if !ok {
		return errUnknownAgent(agent)
	}
	if v != "" && v == s.CLI.OwnLoginProfile || slices.Contains(s.CLI.PinIDs, v) {
		return nil
	}
	if reg == nil || reg.Providers[v] == nil {
		return fmt.Errorf("%s: unknown profile %q", agent, strings.TrimSpace(v))
	}
	return nil
}

// rosterPick fills o's unset Configured, Pins, CanServe and CodexModel from the roster (the router imports no agent).
func rosterPick(o router.PickOpts) (router.PickOpts, error) {
	if o.Configured == nil {
		o.Configured = configured
	}
	if o.Pins == nil {
		pins, err := AgentPins()
		if err != nil {
			return router.PickOpts{}, err
		}
		o.Pins = pins
	}
	if o.CanServe == nil {
		o.CanServe = CanServe
	}
	if o.CodexModel == nil {
		o.CodexModel = func(t providers.Target) providers.Target {
			t, _ = ResolveAgentModel(ModelAsk{Agent: "codex", Target: t})
			return t
		}
	}
	return o, nil
}

// Summary is one agent in `agents`: its binary, availability, capability levels and event kinds.
type Summary struct {
	Name      string            `json:"name"`
	Binary    string            `json:"binary"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Caps      map[string]string `json:"caps"`
	Events    []string          `json:"events"`
	Defaults  Permissions       `json:"permissions"` // the resolved default dispatch policy; {} = none, the tier flags decide
	shown     []string
}

// summaryShown are the capabilities the text roster lists beside each agent.
var summaryShown = []string{"model", "profile", "effort", "workdir", "session", "tools", "system-prompt",
	"append-system-prompt", "env", "output-schema"}

// Summaries describes every registered agent, probing each one's availability.
func Summaries() []Summary {
	var rows []Summary
	for _, r := range Engines() {
		row := Summary{Available: true}
		if err := r.Available(); err != nil {
			row.Available, row.Reason = false, err.Error()
		}
		s, _ := LookupSchema(r.Name())
		row.Caps = map[string]string{}
		for _, f := range capFields(s.CLI.Caps) {
			row.Caps[f.Name] = f.L.String()
			if f.L != None && slices.Contains(summaryShown, f.Name) {
				row.shown = append(row.shown, f.Name+":"+f.L.String())
			}
		}
		row.Caps["cost"], row.Caps["live"] = None.String(), None.String()
		if s.CLI.Cost {
			row.Caps["cost"] = Native.String()
		}
		if s.HasLive() {
			row.Caps["live"] = Native.String()
		}
		row.Caps["background"], row.Caps["remote"] = Native.String(), Native.String()
		row.Binary = s.CLI.Binary
		if !s.External() {
			row.Binary = s.Name
		}
		row.Name, row.Events = s.Name, s.CLI.Events
		var err error
		if row.Defaults, err = DefaultPermissions(s.Name); err != nil { // checkRun refuses every dispatch on it
			row.Available, row.Reason = false, err.Error()
		}
		rows = append(rows, row)
	}
	return rows
}

// Line is the text roster row: availability mark, name, the shown capabilities and the unavailable reason.
func (r Summary) Line() string {
	mark := "✓"
	if !r.Available {
		mark = "✗"
	}
	return fmt.Sprintf("%s %-9s %s %s", mark, r.Name, strings.Join(r.shown, ","), r.Reason)
}

type capField struct {
	Name string
	L    Level
}

// capFields lists an agent's capability levels in RunOptions-flag order, named as the flags are.
func capFields(c Caps) []capField {
	return []capField{{"model", c.Model}, {"profile", c.Provider}, {"effort", c.Effort}, {"workdir", c.WorkDir}, {"session", c.SessionID},
		{"tools", c.Tools}, {"read-only", c.ReadOnly}, {"permissions", c.Permissions}, {"no-tools", c.NoTools}, {"autonomous", c.Autonomous}, {"fleet", c.Fleet}, {"enable-memory", c.Memory},
		{"no-persist", c.NoPersist}, {"safe-mode", c.SafeMode}, {"debug-file", c.DebugFile}, {"direct", c.Direct},
		{"system-prompt", c.SystemPrompt}, {"append-system-prompt", c.AppendSystemPrompt}, {"env", c.Env},
		{"output-schema", c.OutputSchema}, {"persona", c.Persona}}
}
