package project

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Row states of a SyncRow.
const (
	StateCurrent   = "current"
	StateStale     = "stale"
	StateBlocked   = "blocked"
	StatePreserved = "preserved-extra"
)

// SyncRow is one managed destination; it carries paths and names only, never file bodies.
type SyncRow struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Origin  string `json:"origin"` // agent | skill | instructions | extra
	Path    string `json:"path"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Runtime string `json:"runtime"` // always "unverified": sync checks files, not a running agent
}

// rowSink collects Sync's status as SyncRows in JSON mode; other text written to it (the summary) is dropped.
type rowSink struct {
	agent string
	rows  []SyncRow
}

func (s *rowSink) Write(p []byte) (int, error) { return len(p), nil }

// header starts one destination group: a "== name ==" line, or the agent of the rows that follow.
func header(out io.Writer, name string) {
	if s, ok := out.(*rowSink); ok {
		s.agent = name
		return
	}
	fmt.Fprintf(out, "== %s ==\n", name)
}

// status reports one destination: a "  VERB  path (reason)" line, or a SyncRow in JSON mode.
func status(out io.Writer, verb, path, reason string) {
	if s, ok := out.(*rowSink); ok {
		s.rows = append(s.rows, SyncRow{Agent: s.agent, Name: logicalName(path), Origin: originOf(verb, path),
			Path: path, State: verbStates[verb], Reason: reason, Runtime: "unverified"})
		return
	}
	if reason != "" {
		reason = " (" + reason + ")"
	}
	fmt.Fprintf(out, "  %-5s %s%s\n", verb, path, reason)
}

// linkStatus reports a symlink destination with its target: "  VERB  path -> target", or a SyncRow in JSON mode.
func linkStatus(out io.Writer, verb, path, target string) {
	if _, ok := out.(*rowSink); ok {
		status(out, verb, path, "")
		return
	}
	fmt.Fprintf(out, "  %-5s %s -> %s\n", verb, path, target)
}

var verbStates = map[string]string{
	"OK": StateCurrent, "KEEP": StateBlocked, "EXTRA": StatePreserved,
	"DRY": StateStale, "WRITE": StateStale, "LINK": StateStale, "PRUNE": StateStale,
}

// originOf classifies a destination by the verb and the path it sits under.
func originOf(verb, path string) string {
	switch {
	case verb == "EXTRA":
		return "extra"
	case strings.Contains(path, "/skills/"):
		return "skill"
	}
	for _, rel := range instructionFiles {
		if filepath.Base(rel) == filepath.Base(path) {
			return "instructions"
		}
	}
	return "agent"
}

func logicalName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// syncJSON runs Sync with its status recorded as rows, then writes the rows as one JSON document to opts.Out.
func syncJSON(opts Options) int {
	w := opts.Out
	if w == nil {
		w = os.Stdout
	}
	sink := &rowSink{}
	opts.Out = sink
	code := runSync(opts)
	return writeRows(w, sink.rows, code)
}

func writeRows(w io.Writer, rows []SyncRow, code int) int {
	if rows == nil {
		rows = []SyncRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Rows []SyncRow `json:"rows"`
	}{rows}); err != nil {
		return 1
	}
	return code
}
