// jobs.go shapes dispatch rows: one row per job, a carried pair folded into the child doing the work.

package tree

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/models"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

// jobRows flattens ds's node tree into printed rows, so the text and the node tree fold carriers alike.
func jobRows(ds []Dispatch) []nested { return nodeRows(dispatchNodes(ds, 0)) }

// nodeRows flattens the dispatch nodes among nodes into printed rows, each with its ├─/└─ guide.
func nodeRows(nodes []Node) []nested {
	var rows []nested
	var walk func(Node, string, string)
	walk = func(n Node, prefix, guide string) {
		rows = append(rows, nested{Dispatch: *n.Dispatch, guide: guide})
		for i, child := range n.Children {
			branch, next := "├─ ", "│  "
			if i == len(n.Children)-1 {
				branch, next = "└─ ", "   "
			}
			walk(child, prefix+next, prefix+branch)
		}
	}
	for _, n := range nodes {
		if n.Kind == KindDispatch {
			walk(n, "", "")
		}
	}
	return rows
}

// foldInto is the carry row d showing worker w's model, tokens and last event; id, status and elapsed stay d's.
func foldInto(d, w Dispatch) Dispatch {
	if w.Agent != d.Agent && w.takeover { // a carried child is the asked agent; only a takeover names who finished
		d.Agent = d.Agent + "→" + w.Agent
	}
	d.Model, d.Usage, d.Provider = cmp.Or(w.Model, d.Model), w.Usage, cmp.Or(w.Provider, d.Provider)
	d.Runner, d.recType, d.codeMode, d.routeKind = w.Runner, w.recType, w.codeMode, cmp.Or(w.routeKind, d.routeKind)
	d.LastSignal, d.SignalSource, d.Quiet, d.Live = w.LastSignal, w.SignalSource, w.Quiet, w.Live
	d.launch, d.Route, d.Task = cmp.Or(w.launch, d.launch), cmp.Or(w.Route, d.Route), cmp.Or(d.Task, w.Task)
	if d.Status == statusRunning && w.Status == "lost" {
		d.Status = w.Status
	}
	d.Files = append(slices.Clip(d.Files), w.Files...)
	d.Note = cmp.Or(w.Note, d.Note)
	return d
}

// launchLabel is the profile, else provider, the record was launched on: its route, request, then carried line.
func launchLabel(rec agent.Record) string {
	for _, r := range []route.Route{rec.Route, rec.Request.EffectiveRoute(), {Profile: rec.Request.Lead},
		carryRoute(rec.Request.CarryArgv)} {
		if label := cmp.Or(r.Profile, r.Provider); label != "" {
			return label
		}
	}
	return ""
}

// carryRoute is the route a carried inner ask line names with --profile, --provider or an inline --overlay.
func carryRoute(argv []string) route.Route {
	var r route.Route
	for i, a := range argv {
		flag, v, eq := strings.Cut(a, "=")
		if !eq && i+1 < len(argv) {
			v = argv[i+1]
		}
		switch flag {
		case "--profile":
			r.Profile = v
		case "--provider":
			r.Provider = v
		case "--overlay":
			var o route.Route
			if json.Unmarshal([]byte(v), &o) == nil { // a file path does not parse and names nothing here
				r = route.Route{Provider: cmp.Or(r.Provider, o.Provider), Profile: cmp.Or(r.Profile, o.Profile)}
			}
		}
	}
	return r
}

// briefTask is the brief's task text, past an Owned/Mode/Verify header; the whole prompt when it has no Task line.
func briefTask(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	for rest := prompt; rest != ""; {
		line, next, _ := strings.Cut(rest, "\n")
		if body, ok := strings.CutPrefix(strings.TrimSpace(line), "Task:"); ok {
			return strings.TrimSpace(body + "\n" + next)
		}
		rest = next
	}
	return prompt
}

// where is the route a job's row names: its launch, the provider it ran on, the agent's pin, else its own login.
func (d Dispatch) where() string {
	if d.launch != "" || d.Provider != "" {
		return cmp.Or(d.launch, d.Provider)
	}
	launched, _, _ := strings.Cut(d.Agent, "→") // a folded "codex→pi" row was launched on codex's pin
	pinned := vault.GetAgentRecord(launched).EffectiveRoute()
	if pinned.Provider == providers.GroupDatabricks && pinned.Profile == route.AutoArm {
		return pinned.Provider
	}
	return cmp.Or(pinned.Profile, pinned.Provider, "own login")
}

// tokenCells are the "↑in" (input and cache) and "↓out" cells; both blank until the record carries usage.
func (d Dispatch) tokenCells() (up, down string) {
	u := d.Usage
	if u == nil {
		return "", ""
	}
	in := int64(u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens)
	return "↑" + textutil.CompactCount(in), "↓" + textutil.CompactCount(int64(u.OutputTokens))
}

// since is the last-signal age: "lost", "⚠<age>" when quiet, the plain age otherwise, "" with no signal.
func (d Dispatch) since(now time.Time) string {
	if d.Status == "lost" {
		return "lost"
	}
	if d.LastSignal.IsZero() {
		return ""
	}
	age := agent.ShortDur(now.Sub(d.LastSignal))
	if d.Quiet {
		return "⚠" + age
	}
	return age
}

// jobCells are a dispatch row after its marker: agent, model, provider, profile, ↑in, ↓out, since, elapsed.
func jobCells(d Dispatch, now time.Time) []string {
	typ := dispatchType(d)
	if d.Agent == agentid.Pi && !d.codeMode && d.recType != string(models.TypeCoworker) {
		typ = agentid.Pi // a plain pi ask asked no other type: its row reads pi, the stored type stays
	}
	name := wire.RunLabel(typ, d.Runner)
	if d.Fallback != "" && d.Runner == "" {
		name = d.Fallback + "→" + d.Agent + " ⚠"
	}
	model := strings.TrimPrefix(cmp.Or(d.Model, "default"), databricksPrefix)
	provider, profile := d.routeParts()
	up, down := d.tokenCells()
	return []string{name, model, provider, profile, up, down, d.since(now), durationText(d.Duration)}
}

// jobMarker is the job's status glyph.
func jobMarker(d Dispatch) string {
	if d.Status == "lost" {
		return statusGlyph(statusError)
	}
	if d.Status == "ok" {
		return statusGlyph(statusFinished)
	}
	return statusGlyph(d.Status)
}

// addJobs adds the machine-wide dispatch job nodes under a column-name row, one row each; detail adds the lines.
func addJobs(l *layout, nodes []Node, now time.Time, detail bool) {
	l.sep()
	l.add(levelJob, "", "agent", "model", "provider", "", "in", "out", "idle", "time", "")
	l.rows[len(l.rows)-1].head = true
	rows := nodeRows(nodes)
	for _, n := range rows {
		cells := jobCells(n.Dispatch, now)
		cells[0] = n.guide + cells[0]
		addJobRow(l, levelJob, append([]string{jobMarker(n.Dispatch)}, cells...), n.Dispatch, false)
		if detail {
			for _, line := range dispatchExpansion(n.Dispatch) {
				l.free(5, line)
			}
		}
		addHeld(l, 5, n.Files, n.Note)
	}
}

// dispatchExpansion is the two lines under an expanded dispatch row: its task, then status, route, model and id.
func dispatchExpansion(d Dispatch) []string {
	detail := []string{d.Status}
	if d.Fallback != "" {
		detail = append(detail, strings.TrimSpace(d.Fallback+"→"+d.Agent+" ⚠ "+d.FallbackReason))
	}
	detail = append(detail, d.routeText(), strings.TrimPrefix(cmp.Or(d.Model, "default"), databricksPrefix), "id "+d.ID)
	return []string{"└─ " + textutil.OneLine(d.Task), "   " + strings.Join(detail, " · ")}
}

// routeText is "<provider>·<profile>", or what is known of the two, else where().
func (d Dispatch) routeText() string {
	provider, profile := d.routeParts()
	return strings.Trim(provider+"·"+profile, "·")
}

// routeParts are the provider and profile a row names; where() fills the provider when neither is known.
func (d Dispatch) routeParts() (provider, profile string) {
	provider, profile = d.Provider, d.launch
	if head, _, _ := strings.Cut(d.Route, ":"); head != "" {
		provider, profile = head, cmp.Or(profileOf(d.Route), profile)
	} else if reg, err := providers.LoadShared(); err == nil { // a profile-only record: its registry row names the provider
		if row := reg.Providers[cmp.Or(profile, provider)]; row != nil {
			provider, profile = row.Provider, cmp.Or(profile, provider)
		}
	}
	if profile == provider {
		profile = ""
	}
	if provider == "" && profile == "" {
		return d.where(), ""
	}
	return provider, profile
}

// profileOf is the profile of a "provider:profile" route, "" when it names only a provider.
func profileOf(r string) string {
	_, profile, _ := strings.Cut(r, ":")
	return profile
}
