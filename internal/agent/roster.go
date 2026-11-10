package agent

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
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
	case a.Launcher != nil && DefaultLauncher != nil:
		panic("agent: second Launcher registered by " + a.Row.Name)
	case a.Hub != nil && hub != nil:
		panic("agent: second Hub registered by " + a.Row.Name)
	}
	addRow(a.Row)
	drivers[a.Row.Name] = a.Driver
	if a.Launcher != nil {
		DefaultLauncher = a.Launcher
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
	for _, s := range rows() {
		if s.Name == name {
			return cloneSchema(s), true
		}
	}
	return Schema{}, false
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
	spec, known := LookupSchema(agent)
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
