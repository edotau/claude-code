package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent/wire"
	"strings"
)

// Caps declares which RunOptions fields an agent honors; Prompt, Timeout and Attach are always honored.
type Caps struct {
	Model, Provider, Effort, WorkDir, SessionID, Tools, NoTools, Autonomous, Fleet, Memory, NoPersist, SafeMode, DebugFile, Direct Level
	ReadOnly                                                                                                                       Level
	SystemPrompt, AppendSystemPrompt, Env, OutputSchema, Persona                                                                   Level
	Permissions                                                                                                                    Level // reads RunOptions.Permissions
}

// Level is how an agent honors one flag: its own flag (Native), a harness shim (Emulated), or not at all (None).
type Level uint8

const (
	None Level = iota
	Emulated
	Native
)

func (l Level) String() string { return [...]string{"none", "emulated", "native"}[min(l, Native)] }

// MarshalText renders the level as its name, so describe prints "native" rather than 2.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Engine is one agent backend.
type Engine interface {
	Name() string
	Caps() Caps
	Available() error
	Run(ctx context.Context, req RunOptions, sink wire.Sink) (wire.Result, error)
}

// Engines lists every agent in roster order.
func Engines() []Engine {
	var out []Engine
	for _, s := range rows() {
		if e, ok := mustDriver(s.Name).(Engine); ok {
			out = append(out, e)
		} else {
			out = append(out, newCLIEngine(s.Name))
		}
	}
	return out
}

// Lookup finds an agent by name.
func Lookup(name string) (Engine, error) {
	var names []string
	for _, r := range Engines() {
		if r.Name() == name {
			return r, nil
		}
		names = append(names, r.Name())
	}
	return nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(names, "|"))
}

// applyCaps clears fields the agent cannot honor and names each drop; Emulated flags run a harness shim here.
func applyCaps(c Caps, req RunOptions) (out RunOptions, dropped, emulated []string) {
	drop := func(l Level, set bool, flag string, clear func()) {
		if set && l == None {
			dropped = append(dropped, flag)
			clear()
		}
	}
	drop(c.Model, req.Model != "", "--model="+req.Model, func() { req.Model = "" })
	drop(c.Provider, req.Provider != "", "--provider="+req.Provider, func() { req.Provider = "" })
	drop(c.Effort, req.Effort != "", "--effort="+req.Effort, func() { req.Effort = "" })
	drop(c.WorkDir, req.WorkDir != "", "--workdir="+req.WorkDir, func() { req.WorkDir = "" })
	drop(c.SessionID, req.SessionID != "", "--session="+req.SessionID, func() { req.SessionID = "" })
	drop(c.ReadOnly, req.ReadOnly, "--read-only (tools off)", func() { req.ReadOnly, req.Tools = false, false }) // fail closed
	drop(c.Tools, req.Tools, "--tools", func() { req.Tools = false })
	drop(c.NoTools, req.NoTools, "--no-tools", func() { req.NoTools = false })
	drop(c.Autonomous, req.Autonomous, "--autonomous", func() { req.Autonomous = false })
	drop(c.Fleet, req.Fleet, "--fleet", func() { req.Fleet = false })
	drop(c.Memory, req.EnableMemory, "--enable-memory", func() { req.EnableMemory = false })
	drop(c.NoPersist, req.NoPersist, "--no-persist", func() { req.NoPersist = false })
	drop(c.SafeMode, req.SafeMode, "--safe-mode", func() { req.SafeMode = false })
	drop(c.Direct, req.Direct, "--direct", func() { req.Direct = false })
	drop(c.DebugFile, req.DebugFile != "", "--debug-file="+req.DebugFile, func() { req.DebugFile = "" })
	drop(c.Env, len(req.Env) > 0, "--env", func() { req.Env = nil })
	drop(c.Permissions, !req.Permissions.IsZero(), "--allow/--ask/--mode", func() { req.Permissions = Permissions{} })
	drop(c.Persona, req.Persona != nil, "persona="+personaName(req.Persona), func() { req.Persona = nil })
	if req.Persona != nil && c.Persona == Emulated {
		req, dropped = emulatePersona(c, req, dropped)
		emulated = append(emulated, "persona")
	}
	for _, sp := range []struct {
		field *string
		level Level
		flag  string
	}{{&req.SystemPrompt, c.SystemPrompt, "--system-prompt"}, {&req.AppendSystemPrompt, c.AppendSystemPrompt, "--append-system-prompt"}} {
		if *sp.field != "" && sp.level == Emulated {
			req.Prompt, *sp.field = Preamble(*sp.field)+req.Prompt, ""
			emulated = append(emulated, sp.flag)
		}
	}
	drop(c.SystemPrompt, req.SystemPrompt != "", "--system-prompt", func() { req.SystemPrompt = "" })
	drop(c.AppendSystemPrompt, req.AppendSystemPrompt != "", "--append-system-prompt", func() { req.AppendSystemPrompt = "" })
	if req.OutputSchema != "" && c.OutputSchema != Native { // the answer is validated either way; only the instruction differs
		req.Prompt += wire.SchemaInstruction(req.OutputSchema)
		emulated = append(emulated, "--output-schema")
	}
	return req, dropped, emulated
}

// RefuseReadOnly fails closed for an agent whose schema sets RefusesReadOnly: tools-off is no answer for it.
func RefuseReadOnly(c Caps, agentName string, readOnly bool) error {
	s, ok := LookupSchema(agentName)
	if !readOnly || c.ReadOnly != None || !ok || !s.CLI.RefusesReadOnly {
		return nil
	}
	return errors.New(agentName + " has no read-only mode (" + s.CLI.Binary + " cannot restrict tools); use --tools or another agent")
}

// RefusePermissions fails closed when req carries Deny rules (or plan mode) and the agent cannot read Permissions.
func RefusePermissions(c Caps, agentName string, p Permissions) error {
	if c.Permissions != None || (len(p.Deny) == 0 && p.Mode != "plan") {
		return nil
	}
	what := "permission deny rules"
	if len(p.Deny) == 0 {
		what = "--mode plan"
	}
	return errors.New(agentName + " cannot enforce " + what + " yet; dispatch refused")
}
