package project

import (
	"bytes"
	"encoding/json"
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

// rowSink is an io.Writer that turns Sync's status lines into SyncRows.
type rowSink struct {
	buf   bytes.Buffer
	agent string
	rows  []SyncRow
}

func (s *rowSink) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			s.buf.WriteString(line) // keep the partial line for the next write
			break
		}
		s.line(strings.TrimRight(line, "\n"))
	}
	return len(p), nil
}

func (s *rowSink) line(l string) {
	if a, ok := strings.CutPrefix(l, "== "); ok {
		s.agent = strings.TrimSuffix(a, " ==")
		return
	}
	verb, rest, ok := strings.Cut(strings.TrimSpace(l), " ")
	state, known := verbStates[verb]
	if !ok || !known {
		return
	}
	rest = strings.TrimSpace(rest)
	path, reason := rest, ""
	if i := strings.Index(rest, " ("); i > 0 && strings.HasSuffix(rest, ")") {
		path, reason = rest[:i], rest[i+2:len(rest)-1]
	}
	if i := strings.Index(path, " -> "); i > 0 {
		path = path[:i]
	}
	s.rows = append(s.rows, SyncRow{Agent: s.agent, Name: logicalName(path), Origin: originOf(verb, path),
		Path: path, State: state, Reason: reason, Runtime: "unverified"})
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

// syncJSON runs Sync with its status lines captured, then writes the rows as one JSON document to opts.Out.
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
