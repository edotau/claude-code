package hooks

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// The spawn mod's two payload calls ride the dispatch-effort command: top-level `harness_spawn` of "ready" or "query".
const (
	spawnKey   = "harness_spawn"
	spawnReady = "ready"
	spawnQuery = "query"
	readyKind  = "hooks/spawn-mod-ready"
	readyKey   = "ready"
	readyAge   = 7 * 24 * time.Hour
)

// spawnMode is how one dispatch-effort run applies its decisions.
type spawnMode uint8

const (
	modeToday    spawnMode = iota // updatedInput, as the hook always did
	modeDeferred                  // the session's spawn mod applies them: notes only
	modeQuery                     // the mod's spawn-time ask: no recall, no stamps
)

// EffortSpawn is the dispatch-effort runner: the mod's ready and query calls, else the PreToolUse hook.
func EffortSpawn(in *core.Input) core.Result {
	switch in.Nested(spawnKey) {
	case spawnReady:
		return markReady(in.SessionID)
	case spawnQuery:
		return spawnAnswer(in)
	}
	return Effort(in)
}

// markReady records that session's spawn mod is loaded (a refreshed mtime keeps a long session's marker alive).
func markReady(session string) core.Result {
	res := core.Result{Stdout: "{}"}
	if session == "" {
		return res
	}
	if first, err := core.SeenAdd(readyKind, session, readyKey); err != nil {
		res.Stderr = "dispatch-effort: spawn marker not written: " + err.Error()
	} else if !first {
		now := time.Now()
		if err := os.Chtimes(core.SeenFile(readyKind, session), now, now); err != nil {
			res.Stderr = "dispatch-effort: spawn marker not refreshed: " + err.Error()
		}
	}
	return res
}

// modeFor is deferred when the call's session has a marker younger than readyAge, else today's.
func modeFor(in *core.Input) spawnMode {
	if in.SessionID == "" {
		return modeToday
	}
	st, err := os.Stat(core.SeenFile(readyKind, in.SessionID))
	if err != nil || time.Since(st.ModTime()) > readyAge {
		return modeToday
	}
	return modeDeferred
}

// decision tags a decision note as applied at spawn when the mod will apply it; else the note as is.
func (e *effortPlan) decision(note string) string {
	if e.mode != modeDeferred {
		return note
	}
	return strings.Replace(note, "dispatch-effort:", "dispatch-effort (applied at spawn):", 1)
}

// spawnFields maps the Agent input keys the mod's agent.spawn event can rewrite to its field names.
var spawnFields = [][2]string{{"description", "description"}, {"prompt", "prompt"}, {"subagent_type", "subagentType"}, {"model", "model"}}

// spawnAnswer is the one JSON object for the mod: only the fields the decisions change, plus notes; `{}` for none.
func spawnAnswer(in *core.Input) core.Result {
	res := effortFor(context.Background(), in, modeQuery)
	out := map[string]any{}
	if res.Decision == core.Allow {
		maps.Copy(out, changedFields(in.ToolInput(), res.UpdatedInput))
		if notes := spawnNotes(in, res); len(notes) > 0 {
			out["notes"] = notes
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return core.Result{Stdout: "{}", Stderr: "dispatch-effort: spawn answer: " + err.Error()}
	}
	return core.Result{Stdout: string(raw)}
}

// changedFields is the spawn fields whose value in updated differs from orig, under the mod's field names.
func changedFields(orig, updated map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range spawnFields {
		if v, _ := updated[f[0]].(string); v != "" && v != orig[f[0]] {
			out[f[1]] = v
		}
	}
	return out
}

// spawnNotes is the hook's note lines, plus a line for an effort fill the spawn event has no field for.
func spawnNotes(in *core.Input, res core.Result) []string {
	var notes []string
	for l := range strings.SplitSeq(res.Context, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			notes = append(notes, l)
		}
	}
	if e, _ := res.UpdatedInput["effort"].(string); e != "" && e != in.Nested("tool_input", "effort") {
		notes = append(notes, "dispatch-effort: effort "+e+" not applied: no spawn field")
	}
	return notes
}
