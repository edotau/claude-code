package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

const userTurnsPerEdit = 2 // 2 user turns weigh as 1 edit; 6 turns equal updateMinEdits

// sidecarLockWait bounds how long subagentEdits waits for the sidecar lock; a var so tests can shorten it.
var sidecarLockWait = 2 * time.Second

// countState is the transcript count cache; subEdits/subBytes cache subagents' edits against their summed file size.
type countState struct {
	edits    int
	offset   int64
	subEdits int
	subBytes int64
	turns    int
}

func countPath(sessionID string) string {
	return filepath.Join(StateDir(), "count-"+filepath.Base(sessionID))
}

func readCountState(path string) (countState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return countState{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) == 4 {
		return parseCountFields(parts)
	}
	return countState{}, false
}

// parseCountFields decodes the current four-field count line: edits|offset|subEdits:subBytes|turns.
func parseCountFields(parts []string) (countState, bool) {
	edits, err1 := strconv.Atoi(parts[0])
	offset, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || edits < 0 || offset < 0 {
		return countState{}, false
	}
	cs := countState{edits: edits, offset: offset}
	var ok bool
	if cs.subEdits, cs.subBytes, ok = parseSubCache(parts[2]); !ok {
		return countState{}, false
	}
	turns, err := strconv.Atoi(parts[3])
	if err != nil || turns < 0 {
		return countState{}, false
	}
	cs.turns = turns
	return cs, true
}

// parseSubCache decodes the "subEdits:subBytes" field.
func parseSubCache(field string) (subEdits int, subBytes int64, ok bool) {
	subEditsStr, subBytesStr, ok := strings.Cut(field, ":")
	subEdits, err1 := strconv.Atoi(subEditsStr)
	subBytes, err2 := strconv.ParseInt(subBytesStr, 10, 64)
	if !ok || err1 != nil || err2 != nil || subEdits < 0 || subBytes < 0 {
		return 0, 0, false
	}
	return subEdits, subBytes, true
}

func writeCountState(path string, cs countState) {
	line := strconv.Itoa(cs.edits) + "|" + strconv.FormatInt(cs.offset, 10) + "|" +
		strconv.Itoa(cs.subEdits) + ":" + strconv.FormatInt(cs.subBytes, 10) + "|" +
		strconv.Itoa(cs.turns)
	if err := paths.AtomicWrite(path, []byte(line), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "session-update: write count state %s: %v\n", path, err)
	}
}

// CountEdits is countEdits against the session's own count file.
func CountEdits(sessionID, transcriptPath string) int {
	return countEdits(transcriptPath, countPath(sessionID))
}

// countEdits returns work units (edits + turns/2) from the last complete line; subagent files rescan on size moves.
func countEdits(transcriptPath, countFile string) int {
	return countWork(transcriptPath, countFile, true)
}

// readOnlyEdits is countEdits without cache writes, for --dry-run.
func readOnlyEdits(trPath, countFile string) int { return countWork(trPath, countFile, false) }

// countWork is countEdits; write=false counts the same units but persists no cache (the --dry-run view).
func countWork(transcriptPath, countFile string, write bool) int {
	fi, err := os.Stat(transcriptPath)
	if err != nil || fi.IsDir() {
		return 0
	}
	cs, _ := readCountState(countFile)
	if fi.Size() < cs.offset {
		cs = resetCountState(countFile, write)
	}
	mainProgressed := false
	if fi.Size() != cs.offset {
		cs, mainProgressed = advanceMainCount(cs, transcriptPath, countFile, write)
	}
	subEdits, subBytes := subagentEdits(transcriptPath, cs.subBytes, cs.subEdits, write)
	if write && (mainProgressed || subEdits != cs.subEdits || subBytes != cs.subBytes) {
		cs.subEdits, cs.subBytes = subEdits, subBytes
		writeCountState(countFile, cs)
	}
	return cs.edits + subEdits + cs.turns/userTurnsPerEdit
}

// resetCountState drops the cache (the transcript shrank or rotated) and returns a zero state.
func resetCountState(countFile string, write bool) countState {
	if write {
		_ = os.Remove(countFile)
	}
	return countState{}
}

// advanceMainCount folds the transcript bytes appended since cs.offset into cs and reports whether the offset moved.
func advanceMainCount(cs countState, transcriptPath, countFile string, write bool) (countState, bool) {
	deltaEdits, deltaTurns, next := transcript.CountWorkFrom(transcriptPath, cs.offset)
	if next < cs.offset {
		cs = resetCountState(countFile, write)
	}
	cs.edits += deltaEdits
	cs.turns += deltaTurns
	moved := next != cs.offset
	if moved {
		cs.offset = next
	}
	return cs, moved
}

// subFileState is one subagent file's counted offset and edits, cached in the sidecar so a bump rescans only new bytes.
type subFileState struct {
	Offset int64 `json:"offset"`
	Edits  int   `json:"edits"`
}

func sidecarPath(subDir string) string { return filepath.Join(subDir, ".offsets.json") }

func readSidecar(path string) map[string]subFileState {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]subFileState
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func writeSidecar(path string, m map[string]subFileState) {
	if b, err := json.Marshal(m); err == nil {
		_ = paths.AtomicWrite(path, b, 0o600)
	}
}

// subagentEdits recounts agent-*.jsonl edits past cached offsets when their summed size moved; write=false: no lock.
func subagentEdits(transcriptPath string, cachedBytes int64, cachedEdits int, write bool) (edits int, totalBytes int64) {
	dir := inflight.SubagentDir(transcriptPath)
	if dir == "" {
		return 0, 0
	}
	matches, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return cachedEdits, cachedBytes // glob failed: keep the last known count rather than reporting zero
	}
	sizes := make(map[string]int64, len(matches))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil {
			sizes[m] = fi.Size()
			totalBytes += fi.Size()
		}
	}
	if totalBytes == cachedBytes {
		return cachedEdits, totalBytes
	}
	sidecar := sidecarPath(dir)
	if !write {
		return recountSubagents(matches, sizes, sidecar, false), totalBytes
	}
	// Stop and SessionEnd race on one session: serialize so neither regresses the other's offsets; a timeout skips.
	ran, lockErr := paths.TryFileLock(sidecar+".lock", sidecarLockWait, func() error {
		edits = recountSubagents(matches, sizes, sidecar, true)
		return nil
	})
	if ran {
		return edits, totalBytes
	}
	if lockErr != nil {
		// Lock file unopenable (read-only subagents dir): compute unlocked and skip the write, never freeze the count.
		return recountSubagents(matches, sizes, sidecar, false), totalBytes
	}
	// Contention timeout: keep both cached totals; new bytes against stale edits would skip the recount forever.
	return cachedEdits, cachedBytes
}

// recountSubagents sums edits past each cached offset; write=false never persists a stale sidecar (unlocked path).
func recountSubagents(matches []string, sizes map[string]int64, sidecar string, write bool) int {
	prevState := readSidecar(sidecar)
	next := make(map[string]subFileState, len(matches))
	sum := 0
	for _, m := range matches {
		st := prevState[m] // nil map read: zero value, no rescan skipped
		if sizes[m] < st.Offset {
			st = subFileState{} // shrank or rotated: rescan from 0
		}
		if sizes[m] != st.Offset {
			delta, off := transcript.CountEditsFrom(m, st.Offset)
			st.Edits += delta
			st.Offset = off
		}
		sum += st.Edits
		next[m] = st
	}
	if write {
		writeSidecar(sidecar, next)
	}
	return sum
}
