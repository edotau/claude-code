package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// Update gate in work units (edits + turns/2); a re-arm needs 10 new units AND 15 min: chatty re-arms evict history.
const (
	updateMinEdits      = transcript.CommitEditWeight // keeps a `git commit` worth exactly one gate floor
	updateRearmEdits    = 10
	updateRearmCooldown = 15 * time.Minute
	updateMaxBlocks     = 3
)

// Update triggers: the hook that asked for the update.
const (
	TriggerStop       = "stop"
	TriggerPreCompact = "pre-compact"
	TriggerSessionEnd = "session-end"
)

// UpdateRequest is one session's update ask; OnDispatch (optional) runs after each successful worker spawn.
type UpdateRequest struct {
	SessionID, Transcript, CWD string
	OnDispatch                 func(trigger string, work int, now time.Time)
}

// GateResult is the update gate's verdict: Blocked = Stop must block (Work units uncovered); FailedOpen = not saved.
type GateResult struct {
	Blocked    bool
	Work       int
	FailedOpen bool
}

// UpdateIfNew spawns an update when work (edits or user turns) is new since the stamp.
func UpdateIfNew(r UpdateRequest, trigger string, now time.Time) GateResult {
	var blocked, failedOpen bool
	var work int
	ran := false
	err := paths.WithFileLock(stampPath(r.SessionID), func() error { // hooks of one session are separate processes
		ran = true
		blocked, work, failedOpen = updateIfNewLocked(r, trigger, now)
		return nil
	})
	if err != nil && !ran {
		blocked, work, failedOpen = updateIfNewLocked(r, trigger, now) // no lock: unserialized beats skipping
	}
	return GateResult{Blocked: blocked, Work: work, FailedOpen: failedOpen}
}

func updateIfNewLocked(r UpdateRequest, trigger string, now time.Time) (blocked bool, work int, failedOpen bool) {
	stamp := stampPath(r.SessionID)
	st, ok := readStamp(stamp)
	covered := 0
	if ok {
		covered = st.edits
	}
	if trigger == TriggerStop && ok {
		return stampedStop(r, st, stamp, now)
	}
	floor := updateMinEdits
	if trigger == TriggerPreCompact {
		floor = 1
	}
	work = countEdits(r.Transcript, countPath(r.SessionID))
	if work > covered && work >= floor {
		if DispatchUpdate(r, trigger, work, now) {
			return false, 0, false
		}
		if trigger == TriggerStop {
			// Stamp the old covered count: this work was never dispatched, so SessionEnd/PreCompact must still see it.
			if writeStamp(stamp, now, "1", covered) != nil {
				return false, 0, true // fail open: the block count cannot persist
			}
			return true, work, false
		}
	}
	return false, 0, false
}

// stampedStop is the Stop gate for a session that already has a stamp: re-arm, release, or block up to the cap.
func stampedStop(r UpdateRequest, st updateStamp, stamp string, now time.Time) (blocked bool, work int, failedOpen bool) {
	if st.state == "bg" || st.state == "catchup" {
		if now.Sub(st.at) < updateRearmCooldown {
			return false, 0, false // cheap gate before the whole-transcript count
		}
		work = countEdits(r.Transcript, countPath(r.SessionID))
		if work >= st.edits+updateRearmEdits {
			DispatchUpdate(r, TriggerStop, work, now)
		}
		return false, 0, false
	}
	if NewestMtime(Dir(r.CWD)).After(st.at) {
		_ = writeStamp(stamp, now, "bg", st.edits) // ran by hand: released, re-arms; a lost stamp re-dispatches
		return false, 0, false
	}
	if st.blocks >= updateMaxBlocks {
		if st.blocks == updateMaxBlocks { // say so once, at the cap; later Stops stay quiet
			persisted := writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) == nil
			return false, 0, persisted // the notice shows once, only when its stamp persisted
		}
		return false, 0, false // fail open
	}
	if writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits) != nil {
		return false, 0, true // fail open: the block count cannot persist
	}
	// The stamp holds the covered count, not the uncovered work, so the message counts live.
	return true, countEdits(r.Transcript, countPath(r.SessionID)), false
}

// Updatable reports whether a session can be updated: ids present, not an update child, the transcript a file.
func Updatable(sessionID, transcriptPath string) bool {
	if sessionID == "" || transcriptPath == "" || IsUpdateChild() {
		return false
	}
	fi, err := os.Stat(transcriptPath)
	return err == nil && !fi.IsDir()
}

// updateStamp is "<RFC3339>|<bg|block count|catchup>|<edits covered>".
type updateStamp struct {
	at     time.Time
	blocks int
	edits  int
	state  string // raw second field: "bg", "catchup", or a block count
}

func stampPath(sessionID string) string {
	return filepath.Join(StateDir(), "stamp-"+filepath.Base(sessionID))
}

func readStamp(path string) (updateStamp, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return updateStamp{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 {
		return updateStamp{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return updateStamp{}, false
	}
	st := updateStamp{at: at, state: parts[1]}
	// Only a numeric parts[1] is a block count; "bg" or "catchup" leaves st.blocks at zero.
	if n, err := strconv.Atoi(parts[1]); err == nil {
		st.blocks = n
	}
	st.edits, _ = strconv.Atoi(parts[2])
	return st, true
}

func writeStamp(path string, at time.Time, state string, edits int) error {
	err := paths.AtomicWrite(path, []byte(at.Format(time.RFC3339)+"|"+state+"|"+strconv.Itoa(edits)), 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write stamp %s: %v\n", path, err)
	}
	return err
}

// DispatchUpdate spawns the worker and records it; a Stop's spawn also gets a watcher for the session's final state.
func DispatchUpdate(r UpdateRequest, trigger string, work int, now time.Time) bool {
	if !spawn.update(r.Transcript, r.CWD, r.SessionID) {
		return false
	}
	if r.OnDispatch != nil {
		r.OnDispatch(trigger, work, now)
	}
	_ = writeStamp(stampPath(r.SessionID), now, "bg", work) // best effort: a lost stamp only re-dispatches later
	if trigger == TriggerStop {
		spawnIdleWatcher(r.SessionID, r.Transcript, r.CWD)
	}
	return true
}
