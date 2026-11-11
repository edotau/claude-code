package project

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Row states of a syncRow.
const (
	stateCurrent   = "current"
	stateStale     = "stale"
	stateBlocked   = "blocked"
	statePreserved = "preserved-extra"
)

// syncRow is one managed destination; it carries paths and names only, never file bodies.
type syncRow struct {
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
	rows  []syncRow
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

// status reports one destination: a "  VERB  path (reason)" line, or a syncRow in JSON mode.
func status(out io.Writer, verb, path, reason string) {
	if s, ok := out.(*rowSink); ok {
		s.rows = append(s.rows, syncRow{Agent: s.agent, Name: logicalName(path), Origin: originOf(verb, path),
			Path: path, State: verbStates[verb], Reason: reason, Runtime: "unverified"})
		return
	}
	if reason != "" {
		reason = " (" + reason + ")"
	}
	fmt.Fprintf(out, "  %-5s %s%s\n", verb, path, reason)
}

// linkStatus reports a symlink destination with its target: "  VERB  path -> target", or a syncRow in JSON mode.
func linkStatus(out io.Writer, verb, path, target string) {
	if _, ok := out.(*rowSink); ok {
		status(out, verb, path, "")
		return
	}
	fmt.Fprintf(out, "  %-5s %s -> %s\n", verb, path, target)
}

var verbStates = map[string]string{
	"OK": stateCurrent, "KEEP": stateBlocked, "EXTRA": statePreserved,
	"DRY": stateStale, "WRITE": stateStale, "LINK": stateStale, "PRUNE": stateStale,
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

func writeRows(w io.Writer, rows []syncRow, code int) int {
	if rows == nil {
		rows = []syncRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		Rows []syncRow `json:"rows"`
	}{rows}); err != nil {
		return 1
	}
	return code
}
