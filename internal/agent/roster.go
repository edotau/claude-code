package agent

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

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
	Events                []string    // typed event kinds the stream can carry
	Cost                  bool        // reports a cost
	ResumeFormat          string      // "codex resume %s"; "" = none
	ModelsArgv            []string    // args that list models; nil = no catalog verb
	ModelsProvider        string      // list via models --live on this provider instead
	ReadNeedsTools        bool        // file reads are denied without --tools
	PromptInArgv          bool        // prompt rides argv, so MaxPromptBytes applies
	DirFlag               bool        // Headless.WorkDir must be set; cmd.Dir alone is not enough
	Hub                   bool        // ask routes it through the opencode hub unless --native is set
	ConfigureHint         string      // the command to run when no credential resolved
	DisplayName           string      // TUI label; "" = Name
	LoginArgv             []string    // the CLI's own sign-in command
	ExtraUsage            [][2]string // extra help rows
	CatalogUsage          string      // override for the catalog help row
	CatalogHint           string      // extra hint on the catalog page
}

// rosterEntry is a registered row with its roster position.
type rosterEntry struct {
	pos int
	row Schema
}

var (
	registryMu sync.RWMutex
	rosterRows []rosterEntry // sorted by pos; filled by RegisterRow from each agent's init
	drivers    = map[string]Driver{}
)

// RegisterRow adds an agent's roster row at position pos (claude is 0); it runs from an init, never at call time.
func RegisterRow(pos int, row Schema) {
	registryMu.Lock()
	defer registryMu.Unlock()
	i := sort.Search(len(rosterRows), func(i int) bool { return rosterRows[i].pos > pos })
	rosterRows = slices.Insert(rosterRows, i, rosterEntry{pos, row})
}

// Register adds an external CLI's Driver under its Name.
func Register(d Driver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	drivers[d.Name()] = d
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
	out := make([]Schema, len(rosterRows))
	for i, e := range rosterRows {
		out[i] = e.row
	}
	return out
}

// Roster returns independent copies of the roster rows, claude first, with external wire facts filled in.
func Roster() []Schema {
	rs := rows()
	for i := range rs {
		s := &rs[i]
		s.Tools = slices.Clone(s.Tools)
		s.CLI.Dialects = slices.Clone(s.CLI.Dialects)
		s.CLI.RouterSuffix = maps.Clone(s.CLI.RouterSuffix)
		s.CLI.Events = slices.Clone(s.CLI.Events)
		s.CLI.ModelsArgv = slices.Clone(s.CLI.ModelsArgv)
		s.CLI.LoginArgv = slices.Clone(s.CLI.LoginArgv)
		s.CLI.ExtraUsage = slices.Clone(s.CLI.ExtraUsage)
	}
	return rs
}

// LookupSchema returns a roster row by name, claude included.
func LookupSchema(name string) (Schema, bool) {
	for _, s := range Roster() {
		if s.Name == name {
			return s, true
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
	for _, n := range reg.ProfilesOf(s.CLI.ModelsProvider) {
		if reg.Providers[n].Auth.Login == s.Name {
			return n, nil
		}
	}
	return "", fmt.Errorf("%s: no %s profile with a %s login", s.Name, s.CLI.ModelsProvider, s.Name)
}
