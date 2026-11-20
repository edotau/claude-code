package agent

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// cutoffSchema versions the recent-cutoffs file; cutoffWindow is how long an entry steers a resubmit.
const (
	cutoffSchema = 1
	cutoffWindow = 60 * time.Minute
	// recallMark starts the memory-recall block ask appends to a brief; it is not part of the task being matched.
	recallMark = "\n\n<memory-recall>"
)

// CutoffEntry is one recent cut-off run: the hash of its task, never the task itself.
type CutoffEntry struct {
	Key   string    `json:"key"`
	ID    string    `json:"id"`
	Kind  string    `json:"kind"`
	Agent string    `json:"agent"` // the agent that was cut off (the fallback's, when one ran)
	Time  time.Time `json:"time"`
}

type cutoffFile struct {
	Schema  int           `json:"schema"`
	Entries []CutoffEntry `json:"entries"`
}

func cutoffPath() string { return filepath.Join(paths.StateDir(), "agent", "cutoffs.json") }

var cutoffFormat = schemas.Register(schemas.Format{
	Name: "dispatch-cutoffs", Owner: "internal/agent", Kind: schemas.Cache, Version: cutoffSchema,
	Glob: cutoffPath,
	New:  func() any { return new(cutoffFile) },
	Keep: "rebuildable cache; entries expire after 60 minutes and a version mismatch is a miss",
})

// CutoffKey hashes agent, model selector and task (memory-recall block and trailing whitespace dropped).
func CutoffKey(agentName, model, prompt string) string {
	task, _, _ := strings.Cut(prompt, recallMark)
	sum := sha256.Sum256([]byte(agentName + "\x00" + model + "\x00" + strings.TrimRight(task, " \t\r\n")))
	return hex.EncodeToString(sum[:])
}

// readCutoffs returns the file's entries; a missing, unreadable or mismatched file is empty.
func readCutoffs() []CutoffEntry {
	b, err := os.ReadFile(cutoffPath())
	if err != nil {
		return nil
	}
	var f cutoffFile
	if !schemas.DecodeOK(cutoffFormat, b, &f) {
		return nil
	}
	return f.Entries
}

// RecentCutoff is the newest entry for key within the window; it reads one small file and nothing else.
func RecentCutoff(key string, now time.Time) (CutoffEntry, bool) {
	var best CutoffEntry
	for _, e := range readCutoffs() {
		if e.Key == key && now.Sub(e.Time) < cutoffWindow && e.Time.After(best.Time) {
			best = e
		}
	}
	return best, best.Key != ""
}

// updateCutoffs rewrites the file under its lock: expired entries dropped, then edit applied.
func updateCutoffs(now time.Time, edit func([]CutoffEntry) []CutoffEntry) error {
	path := cutoffPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(path, func() error {
		kept := slices.DeleteFunc(readCutoffs(), func(e CutoffEntry) bool { return now.Sub(e.Time) >= cutoffWindow })
		b, err := json.Marshal(cutoffFile{Schema: cutoffSchema, Entries: edit(kept)})
		if err != nil {
			return err
		}
		return paths.AtomicWrite(path, b, 0o600)
	})
}

// NoteCutoff files a cut-off run under its task hash, or clears the hash after a success on the same agent.
func (r Record) NoteCutoff(now time.Time) error {
	key := CutoffKey(r.Agent, r.Request.Model, r.Request.Prompt)
	switch {
	case r.CutOff():
		e := CutoffEntry{Key: key, ID: r.ID, Kind: r.Cutoff, Agent: cmp.Or(r.RanAs, r.Agent), Time: now}
		return updateCutoffs(now, func(es []CutoffEntry) []CutoffEntry {
			return append(slices.DeleteFunc(es, func(x CutoffEntry) bool { return x.Key == key }), e)
		})
	case r.Status == "ok" && (r.RanAs == "" || r.RanAs == r.Agent):
		if !slices.ContainsFunc(readCutoffs(), func(e CutoffEntry) bool { return e.Key == key }) {
			return nil // the common success writes nothing
		}
		return updateCutoffs(now, func(es []CutoffEntry) []CutoffEntry {
			return slices.DeleteFunc(es, func(x CutoffEntry) bool { return x.Key == key })
		})
	}
	return nil
}

// warnCutoff reports a failed cutoff-file write once; the dispatch itself never fails on it.
func (r Record) warnCutoff(notify func(string)) {
	if err := r.NoteCutoff(time.Now()); err != nil {
		msg := fmt.Sprintf("dispatch %s: recent-cutoffs file not updated: %v", r.ID, err)
		if notify != nil {
			notify(msg)
			return
		}
		fmt.Fprintln(NoticeOut, "claude-code: "+msg)
	}
}
