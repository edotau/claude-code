// Package roles runs a task by role (explore, fleet, compare): a role names an agent, legs fan out under one limit.
package roles

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// reportSchema versions the Report JSON.
const reportSchema = 1

// streamLeg is the per-leg dispatch seam for Run.
var streamLeg = (*agent.Client).Stream

// Report is a role run: Status is ok (all legs ok), partial (at least one) or error (none).
type Report struct {
	Schema int         `json:"schema"`
	Role   string      `json:"role"`
	Agent  string      `json:"agent"`
	Status string      `json:"status"`
	Legs   []LegResult `json:"legs"`
}

// LegResult is one leg's label and its `ask --format json` envelope.
type LegResult struct {
	Label    string        `json:"label"`
	Envelope wire.Envelope `json:"envelope"`
}

// Run dispatches legs concurrently, host-isolated under min(jobs, 12), tagging progress lines with the label.
func Run(ctx context.Context, c *agent.Client, r Role, legs []Leg, jobs int, progress io.Writer) Report {
	return RunWith(ctx, r, legs, jobs, progress, func(ctx context.Context, o agent.RunOptions) <-chan wire.Event {
		return streamLeg(c, ctx, o)
	})
}

// Runner streams one leg's events, ending with its result event; the caller injects retry policy through it.
type Runner func(context.Context, agent.RunOptions) <-chan wire.Event

// RunWith is Run with the per-leg runner injected.
func RunWith(ctx context.Context, r Role, legs []Leg, jobs int, progress io.Writer, stream Runner) Report {
	ctx = agent.WithBatchRun(ctx, retry.NewBatchLimiter(min(jobs, MaxParts)))
	rep := Report{Schema: reportSchema, Role: r.Name, Agent: r.Agent, Legs: make([]LegResult, len(legs))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, leg := range legs {
		wg.Go(func() {
			rep.Legs[i] = runLeg(ctx, leg, progress, &mu, stream)
		})
	}
	wg.Wait()
	rep.Status = status(rep.Legs)
	return rep
}

func runLeg(ctx context.Context, leg Leg, progress io.Writer, mu *sync.Mutex, stream Runner) LegResult {
	if leg.Err != "" {
		return LegResult{Label: leg.Label, Envelope: failed(leg, leg.Err)}
	}
	env := failed(leg, "no result event")
	var line []byte // text events are chunks: only whole lines are tagged and written, so legs never interleave mid-line
	flush := func(b []byte) {
		mu.Lock()
		_, _ = fmt.Fprintf(progress, "[%s] %s\n", leg.Label, b)
		mu.Unlock()
	}
	for ev := range stream(ctx, leg.Opts) {
		switch {
		case ev.Type == wire.EventText && progress != nil:
			line = progressLines(line, ev, flush)
		case ev.Type == wire.EventResult && ev.Envelope != nil:
			env = *ev.Envelope
		}
	}
	if len(line) > 0 {
		flush(line)
	}
	return LegResult{Label: leg.Label, Envelope: env}
}

func progressLines(line []byte, ev wire.Event, flush func([]byte)) []byte {
	if ev.Sep && len(line) > 0 {
		line = append(line, '\n')
	}
	line = append(line, ev.Text...)
	for i := bytes.IndexByte(line, '\n'); i >= 0; i = bytes.IndexByte(line, '\n') {
		flush(line[:i])
		line = line[i+1:]
	}
	return line
}

func failed(leg Leg, msg string) wire.Envelope {
	env := wire.FailureEnvelope(leg.Opts.Agent, fmt.Errorf("%s", msg))
	if env.Model == "" {
		env.Model = leg.Opts.Model
	}
	return env
}

func status(legs []LegResult) string {
	ok := 0
	for _, l := range legs {
		if l.Envelope.Status == "ok" {
			ok++
		}
	}
	switch ok {
	case len(legs):
		return "ok"
	case 0:
		return "error"
	}
	return "partial"
}

// Text prints each leg as `### <label> (<agent>/<model>) [<status>]` with its answer or error, then failed labels.
func (rep Report) Text(w io.Writer) error {
	bw := bufio.NewWriter(w)
	var bad []string
	for _, l := range rep.Legs {
		e := l.Envelope
		_, _ = fmt.Fprintf(bw, "### %s (%s/%s) [%s]\n", l.Label, e.Agent, e.Model, e.Status)
		if e.Status == "ok" {
			_, _ = fmt.Fprintln(bw, e.Answer)
			continue
		}
		bad = append(bad, l.Label)
		if e.Error != nil {
			_, _ = fmt.Fprintln(bw, e.Error.Message)
		}
	}
	if len(bad) > 0 {
		_, _ = fmt.Fprintf(bw, "failed: %s\n", strings.Join(bad, ", "))
	}
	return bw.Flush()
}

// Request is the verb's input; Parts are explore sub-questions, Models override a compare role's default set.
type Request struct {
	Task, WorkDir string
	Parts, Models []string
	Web           bool
	Timeout       time.Duration
	OutputSchema  string
	Fallback      string               // --fallback, else the overlay's
	Permissions   agent.Permissions    // --allow/--ask/--deny/--mode, added over the role's own
	Overlay       agentconfig.Resolved // the overlay's env layer
	Lead, Profile string               // the narrowed lead and --provider (ask's resolveLead), applied to every leg
	Auto          string               // the auto-picked Databricks workspace (profile auto), else ""
}

// layer is base plus add: rules union, add's mode wins when set.
func layer(base, add agent.Permissions) agent.Permissions {
	out := agent.Permissions{Mode: cmp.Or(add.Mode, base.Mode), Allow: slices.Clone(base.Allow), Ask: slices.Clone(base.Ask),
		Deny: slices.Clone(base.Deny)}
	for _, r := range add.Allow {
		if !slices.Contains(out.Allow, r) {
			out.Allow = append(out.Allow, r)
		}
	}
	for _, r := range add.Ask {
		if !slices.Contains(out.Ask, r) {
			out.Ask = append(out.Ask, r)
		}
	}
	for _, r := range add.Deny {
		if !slices.Contains(out.Deny, r) {
			out.Deny = append(out.Deny, r)
		}
	}
	return out
}

// Leg is one dispatch of a role; a non-empty Err means it failed before dispatch.
type Leg struct {
	Label string
	Opts  agent.RunOptions
	Err   string
}

// Legs builds r's legs for q; the shape follows the role name, the agent comes from the table.
func Legs(r Role, q Request) []Leg {
	base := agent.RunOptions{Agent: r.Agent, WorkDir: q.WorkDir, Timeout: q.Timeout, OutputSchema: q.OutputSchema,
		Permissions: layer(r.Permissions, q.Permissions), Fallback: q.Fallback, Lead: q.Lead, Provider: q.Profile,
		AutoArm: q.Auto != ""}.ApplyOverlay(q.Overlay)
	var legs []Leg
	switch r.Name {
	case "explore":
		legs = exploreLegs(base, q)
	case "fleet":
		o := base
		o.Fleet, o.Tools, o.Prompt = true, true, q.Task
		legs = []Leg{{Label: "fleet", Opts: o}}
	case "compare":
		legs = compareLegs(r, base, q)
	}
	for i, leg := range legs {
		if leg.Err != "" {
			continue
		}
		if err := agent.CheckCapabilities(leg.Opts.Agent, leg.Opts); err != nil { // refuse before dispatch, never drop a flag
			legs[i].Err = err.Error()
		}
	}
	return legs
}

func exploreLegs(base agent.RunOptions, q Request) []Leg {
	parts := q.Parts
	if len(parts) == 0 {
		parts = []string{q.Task}
	}
	legs := make([]Leg, 0, len(parts))
	for _, p := range parts {
		o, err := options(base, p, q.Web)
		leg := Leg{Label: p, Opts: o}
		if err != nil { // keep the requested agent for the failure envelope; Options returns zeroed opts on refusal
			leg.Opts, leg.Err = base, err.Error()
		}
		legs = append(legs, leg)
	}
	return legs
}

func compareLegs(r Role, base agent.RunOptions, q Request) []Leg {
	models := q.Models
	if len(models) == 0 {
		models = r.Models
	}
	menu := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		menu[m.ID] = true
	}
	legs := make([]Leg, 0, len(models))
	for _, m := range models {
		o := base
		o.Model, o.Prompt = m, q.Task
		leg := Leg{Label: m, Opts: o}
		if r.Agent == "opencode" && !menu[m] {
			leg.Err = "not on the opencode menu"
		}
		legs = append(legs, leg)
	}
	return legs
}

// rolesKey is the vault Overlay entry holding the user's role table: a JSON array of Role.
const rolesKey = "dispatch-roles"

// Role maps a role name to the agent that runs it; Models is the default model set for compare.
type Role struct {
	Name        string            `json:"name"`
	Agent       string            `json:"agent"`
	Models      []string          `json:"models,omitempty"`
	Permissions agent.Permissions `json:"permissions,omitzero"` // the role layer, over the agent's defaults
}

// seed is the built-in role table, replaced by the vault overlay when set.
var seed = []Role{
	{Name: "explore", Agent: "copilot"}, // read-only enforced; gemini/opencode explore is refused (owner, 2026-11-09)
	{Name: "fleet", Agent: "copilot"},
	{Name: "compare", Agent: "opencode", Models: []string{"claude-sonnet-5-5", "gpt-6-1-sol", "gemini-3-8-flash", "kimi-k3"}},
}

// Lookup finds name in the vault overlay table if set and valid, else in seed.
func Lookup(name string) (Role, error) {
	table := seed
	t, err := vaultRoles()
	if err != nil {
		return Role{}, err
	}
	if len(t) > 0 {
		table = t
	}
	names := make([]string, 0, len(table))
	for _, r := range table {
		if r.Name == name {
			return r, nil
		}
		names = append(names, r.Name)
	}
	return Role{}, fmt.Errorf("unknown role %q (%s)", name, strings.Join(names, "|"))
}

// vaultRoles is the overlay table, or nil when none is set; an unreadable vault or malformed JSON is an error.
func vaultRoles() ([]Role, error) {
	var out []Role
	var perr error
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, rolesKey)
		if !ok || raw == "" {
			return nil
		}
		if jerr := json.Unmarshal([]byte(raw), &out); jerr != nil {
			perr = fmt.Errorf("vault overlay %s is not a JSON array of roles: %w", rolesKey, jerr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read vault role overlay: %w", err)
	}
	if perr != nil {
		return nil, perr
	}
	return out, nil
}

// promptBase is the read-only Explore brief shared by every agent; web is appended when enabled.
const promptBase = `Read-only: never modify, create or delete a file.
Orient first: list the directories that matter, read the README/spec for the area, then read narrowly.
Cite file:line for every claim about the code.`

// promptWeb appends the web-research dispatch, cited by URL.
const promptWeb = `
Then research the relevant libraries, APIs and prior art on the web, citing a URL for every claim.
If a source can't be reached, say so instead of guessing.`

// promptTail fixes the five required sections and the stopping rule.
const promptTail = `
Output exactly these five sections, each a few bullets: Structure, Conventions, Dependencies, Prior art,
Open questions. Stop once two more lookups wouldn't change the brief.

Question: `

// prompt builds the Codebase Context Brief prompt for question; web adds the web-research dispatch.
func prompt(question string, web bool) string {
	p := promptBase
	if web {
		p += promptWeb
	}
	return p + promptTail + question
}

// needsTools reports whether the agent must run with tools on to read the repo.
func needsTools(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.ReadNeedsTools
}

// enforcesReadOnly reports whether the agent's own CLI enforces --read-only (not every vendor can).
func enforcesReadOnly(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.Caps.ReadOnly == agent.Native
}

// requireReadOnly refuses an agent that reads with tools but cannot enforce read-only (prompt-only is not enough).
func requireReadOnly(agentName string) error {
	if !needsTools(agentName) || enforcesReadOnly(agentName) {
		return nil
	}
	return fmt.Errorf("explore on %s needs native read-only support, which it emulates with permission denies; use codex or copilot", agentName)
}

// Args builds the `ask` argv for an explore run: --tools only when the agent needs it to read.
func Args(agentName, cwd, question string, web, background bool) ([]string, error) {
	if err := requireReadOnly(agentName); err != nil {
		return nil, err
	}
	args := []string{"--agent", agentName, "--workdir", cwd}
	if needsTools(agentName) {
		args = append(args, "--tools", "--read-only")
	}
	if background {
		args = append(args, "--background")
	}
	return append(args, prompt(question, web)), nil
}

// options returns base with the explore prompt, tools and read-only on; it refuses prompt-only agents.
func options(base agent.RunOptions, question string, web bool) (agent.RunOptions, error) {
	if err := requireReadOnly(base.Agent); err != nil {
		return agent.RunOptions{}, err
	}
	o := base
	o.Prompt = prompt(question, web)
	if needsTools(o.Agent) {
		o.Tools, o.ReadOnly = true, true
	}
	return o, nil
}
