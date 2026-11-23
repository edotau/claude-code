// jobs.go shapes dispatch rows: one row per job, a carried pair folded into the child doing the work.

package tree

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
)

// jobRows flattens ds's node tree into printed rows, so the text and the node tree fold carriers alike.
func jobRows(ds []Dispatch) []nested {
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
	for _, n := range dispatchNodes(ds, 0) {
		walk(n, "", "")
	}
	return rows
}

// foldInto is the carry row d showing worker w's model, tokens and last event; id, status and elapsed stay d's.
func foldInto(d, w Dispatch) Dispatch {
	if w.Agent != d.Agent {
		d.Agent = d.Agent + "→" + w.Agent
	}
	d.Model, d.Usage, d.Provider = cmp.Or(w.Model, d.Model), w.Usage, cmp.Or(w.Provider, d.Provider)
	d.LastSignal, d.SignalSource, d.Quiet, d.Live = w.LastSignal, w.SignalSource, w.Quiet, w.Live
	d.launch, d.Route, d.Task = cmp.Or(w.launch, d.launch), cmp.Or(w.Route, d.Route), cmp.Or(d.Task, w.Task)
	if w.queued != "" {
		d.queued, d.queuedAt = w.queued, w.queuedAt
	}
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
	pinned := providers.AgentRecord(launched).EffectiveRoute()
	if pinned.Provider == providers.GroupDatabricks && pinned.Profile == route.AutoArm {
		return pinned.Provider
	}
	return cmp.Or(pinned.Profile, pinned.Provider, "own login")
}

// tokenCells are the "↑in" (input and cache) and "↓out" cells; "↑-" "↓-" until the record carries usage.
func (d Dispatch) tokenCells() (up, down string) {
	u := d.Usage
	if u == nil {
		return "↑-", "↓-"
	}
	in := int64(u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens)
	return "↑" + textutil.CompactCount(in), "↓" + textutil.CompactCount(int64(u.OutputTokens))
}

// jobCells are a dispatch row after its marker: agent, model · route, ↑in, ↓out, since its last event, elapsed.
func jobCells(d Dispatch, now time.Time, pools map[string]serve.PoolRow) []string {
	name := d.Agent
	if d.Fallback != "" {
		name = d.Fallback + "→" + d.Agent + " ⚠"
	}
	model := strings.TrimPrefix(cmp.Or(d.Model, "default"), databricksPrefix) + " · " + d.where()
	up, down := d.tokenCells()
	if d.queued != "" {
		up, down = queueText(d, now, pools), ""
	}
	since := ""
	if d.Status == "lost" {
		since = "lost"
	} else if !d.LastSignal.IsZero() {
		since = shortDuration(now.Sub(d.LastSignal))
		if d.Quiet {
			since = "⚠" + since
		}
	}
	return []string{name, model, up, down, since, durationText(d.Duration)}
}

// queueText is "queued 1m10s (pool databricks:qa 2/4)"; the count is left out when the daemon does not answer.
func queueText(d Dispatch, now time.Time, pools map[string]serve.PoolRow) string {
	pool := d.queued
	if p, ok := pools[d.queued]; ok && p.Cap > 0 {
		pool = fmt.Sprintf("%s %d/%d", pool, p.Running, p.Cap)
	}
	return "queued " + shortDuration(now.Sub(d.queuedAt)) + " (pool " + pool + ")"
}

// jobMarker is "○" for a job waiting on admission, else the status glyph.
func jobMarker(d Dispatch) string {
	if d.queued != "" {
		return "○"
	}
	if d.Status == "lost" {
		return statusGlyph(statusError)
	}
	return statusGlyph(d.Status)
}

// queryPools is the serve daemon's pool view; a test swaps it so no socket is dialed.
var queryPools = func() map[string]serve.PoolRow {
	st, err := serve.QueryStatus(300 * time.Millisecond)
	if err != nil {
		return nil
	}
	pools := make(map[string]serve.PoolRow, len(st.Pools))
	for _, p := range st.Pools {
		pools[p.Pool] = p
	}
	return pools
}

// poolsFor asks the daemon for its pools only when one of rows is queued, so a plain listing dials nothing.
func poolsFor(rows []nested) map[string]serve.PoolRow {
	for _, n := range rows {
		if n.queued != "" {
			return queryPools()
		}
	}
	return nil
}

// addJobs adds the machine-wide dispatch jobs under a "dispatches" header, one row each; detail adds id and task.
func addJobs(l *layout, dispatches []Dispatch, now time.Time, detail bool, selected string) {
	if len(dispatches) == 0 {
		return
	}
	l.sep()
	l.free(0, "dispatches")
	rows := jobRows(dispatches)
	pools := poolsFor(rows)
	for _, n := range rows {
		cells := jobCells(n.Dispatch, now, pools)
		cells[0] = n.guide + cells[0]
		l.add(levelJob, append(append([]string{jobMarker(n.Dispatch)}, cells...), "")...)
		l.mark(n.Status == statusRunning, 1<<0)
		l.spanLast(n.queued != "", selected != "" && n.ID == selected)
		if detail {
			l.free(5, joinText(n.ID, "  ", textutil.OneLine(n.Task)))
		}
		addHeld(l, 5, n.Files, n.Note)
	}
}
