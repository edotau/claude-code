package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Record is one `ask` dispatch: written as "running" before the run, so a killed run stays visible.
type Record struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Agent     string    `json:"agent"`
	Request   Request   `json:"request"`
	Status    string    `json:"status"` // running | ok | error
	Error     string    `json:"error,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Duration  string    `json:"duration,omitempty"`
}

// dispatchKeep bounds the log; the oldest records beyond it are pruned on every write.
var dispatchKeep = 200

func dispatchDir() string { return filepath.Join(paths.StateDir(), "dispatch") }

// NewRecord stamps an id that sorts by time and names the agent: 20261016T210512.123-codex.
func NewRecord(agent string, req Request, now time.Time) Record {
	return Record{ID: now.UTC().Format("20060102T150405.000") + "-" + agent, Time: now.UTC(), Agent: agent, Request: req, Status: "running"}
}

// Save writes the record (0600) and prunes past dispatchKeep. The prompt is kept verbatim so a retry is exact.
func (r Record) Save() error {
	dir := dispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := paths.AtomicWrite(filepath.Join(dir, r.ID+".json"), b, 0o600); err != nil {
		return err
	}
	pruneDispatches(dir)
	return nil
}

// Finish records the outcome of the run.
func (r *Record) Finish(res Result, err error, took time.Duration) {
	r.Status, r.Provider, r.Model, r.SessionID = "ok", res.Provider, res.Model, res.SessionID
	r.Duration = took.Round(time.Millisecond).String()
	if err != nil {
		r.Status, r.Error = "error", err.Error()
	}
}

func pruneDispatches(dir string) {
	names := recordNames(dir)
	for len(names) > dispatchKeep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// recordNames lists record files oldest first (ids sort by time).
func recordNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// LoadRecord reads one record by id; a unique id prefix is accepted.
func LoadRecord(id string) (Record, error) {
	var match []string
	for _, n := range recordNames(dispatchDir()) {
		if strings.HasPrefix(n, id) {
			match = append(match, n)
		}
	}
	if len(match) != 1 {
		return Record{}, fmt.Errorf("dispatch %q: %d matches (see claude-code ask --log)", id, len(match))
	}
	var r Record
	b, err := os.ReadFile(filepath.Join(dispatchDir(), match[0]))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

// RecentRecords returns up to n records, newest first.
func RecentRecords(n int) []Record {
	names := recordNames(dispatchDir())
	var out []Record
	for i := len(names) - 1; i >= 0 && len(out) < n; i-- {
		if r, err := LoadRecord(strings.TrimSuffix(names[i], ".json")); err == nil {
			out = append(out, r)
		}
	}
	return out
}
