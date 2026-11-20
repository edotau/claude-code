package hooks

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
)

// The spawn mod's two payload calls ride the dispatch-effort command: top-level `harness_spawn` of "ready" or "query".
const (
	spawnKey   = "harness_spawn"
	spawnReady = "ready"
	spawnQuery = "query"
	readyKind  = "hooks/spawn-mod-ready"
	readyAge   = 7 * 24 * time.Hour
)

// spawnMode is how one dispatch-effort run applies its decisions.
type spawnMode uint8

const (
	modeToday    spawnMode = iota // updatedInput, as the hook always did
	modeDeferred                  // the session's spawn mod applies them: denials, retype and recall only
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

// readyOwner is the marker key: the agent process, so a resumed session in a new process never matches.
func readyOwner() string {
	return strconv.Itoa(inflight.SessionOwner())
}

// markReady records that session's spawn mod is loaded (a refreshed mtime keeps a long session's marker alive).
func markReady(session string) core.Result {
	res := core.Result{Stdout: "{}"}
	if session == "" || readyOwner() == "0" {
		return res
	}
	if first, err := core.SeenAdd(readyKind, session, readyOwner()); err != nil {
		res.Stderr = "dispatch-effort: spawn marker not written: " + err.Error()
	} else if !first {
		now := time.Now()
		if err := os.Chtimes(core.SeenFile(readyKind, session), now, now); err != nil {
			res.Stderr = "dispatch-effort: spawn marker not refreshed: " + err.Error()
		}
	}
	return res
}

// modeFor is deferred when this process's mod marked the session within readyAge, else today's.
func modeFor(in *core.Input) spawnMode {
	if in.SessionID == "" {
		return modeToday
	}
	st, err := os.Stat(core.SeenFile(readyKind, in.SessionID))
	if err != nil || time.Since(st.ModTime()) > readyAge {
		return modeToday
	}
	if seen, _ := core.SeenHas(readyKind, in.SessionID, readyOwner()); !seen {
		return modeToday
	}
	return modeDeferred
}

// spawnFields maps the Agent input keys the mod's agent.spawn event can rewrite to its field names.
var spawnFields = [][2]string{{"description", "description"}, {"prompt", "prompt"}, {"subagent_type", "subagentType"}, {"model", "model"}}

// spawnAnswer is the one JSON object for the mod: only the fields the decisions change; `{}` for none.
func spawnAnswer(in *core.Input) core.Result {
	res := effortFor(context.Background(), in, modeQuery)
	out := map[string]any{}
	if res.Decision == core.Allow {
		maps.Copy(out, changedFields(in.ToolInput(), res.UpdatedInput))
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
