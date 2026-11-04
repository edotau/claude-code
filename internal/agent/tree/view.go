package tree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/transcript"
)

// defaultEventCount is --n's default: how many trailing events a Snapshot carries.
const defaultEventCount = 40

// followPollInterval is how often Follow re-reads the transcript, journal and meta.
var followPollInterval = 500 * time.Millisecond

// ViewOptions scopes View/Follow like Options, narrowed to one Agent (full id or unique prefix).
type ViewOptions struct {
	ClaudeDir string
	Session   string
	Days      int
	Agent     string
	N         int // trailing events kept in Snapshot.Events; <=0 -> defaultEventCount
	Now       func() time.Time
}

func (o ViewOptions) now() time.Time { return nowOr(o.Now) }

// Event is one line of a subagent event stream: a tool call, an assistant text blurb, or the journal result.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // tool|text|result
	Tool string    `json:"tool,omitempty"`
	Arg  string    `json:"arg,omitempty"`
	Text string    `json:"text,omitempty"`
	Size int       `json:"size,omitempty"` // result payload bytes, kind=="result" only
}

// Snapshot is one agent's drill-down view: `claude-code network view <id>`.
type Snapshot struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"sessionId"`
	Label       string     `json:"label"`
	Workflow    string     `json:"workflow,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	Model       string     `json:"model,omitempty"`
	Effort      string     `json:"effort,omitempty"`
	Status      string     `json:"status"`
	Updated     time.Time  `json:"updated"`
	Tokens      Tokens     `json:"tokens"`
	FilesEdited []string   `json:"filesEdited,omitempty"`
	Dispatches  []Dispatch `json:"dispatches,omitempty"`
	Activity    *Activity  `json:"activity,omitempty"`
	Result      string     `json:"result,omitempty"`
	Events      []Event    `json:"events,omitempty"`
}

// agentLocation is where a resolved agent's transcript lives: its session and workflow run dir ("" if plain).
type agentLocation struct {
	SessionID      string
	TranscriptPath string
	SessionDir     string
	RunDir         string
	RunID          string
	Cwd            string
}

// View resolves opts.Agent to one subagent transcript and builds its drill-down snapshot.
func View(opts ViewOptions) (Snapshot, error) {
	now := opts.now()
	loc, err := resolveAgent(opts.ClaudeDir, opts, now)
	if err != nil {
		return Snapshot{}, err
	}
	n := opts.N
	if n <= 0 {
		n = defaultEventCount
	}
	return snapshotFrom(loc, now, n), nil
}

// resolveAgent finds the one agent in scope whose id matches opts.Agent exactly or as a unique prefix.
func resolveAgent(claudeDir string, opts ViewOptions, now time.Time) (agentLocation, error) {
	if opts.Agent == "" {
		return agentLocation{}, errors.New("view: no agent id given")
	}
	sessionFiles, err := sessionTranscriptsInScope(claudeDir, Options{Days: opts.Days, Session: opts.Session}, now)
	if err != nil {
		return agentLocation{}, err
	}
	var matches []agentLocation
	for _, transcriptPath := range sessionFiles {
		matches = append(matches, sessionAgentMatches(transcriptPath, opts.Agent)...)
	}
	if len(matches) > 1 { // an exact id wins over longer ids it prefixes
		var exact []agentLocation
		for _, m := range matches {
			if subagentIDFromPath(m.TranscriptPath) == opts.Agent {
				exact = append(exact, m)
			}
		}
		if len(exact) == 1 {
			matches = exact
		}
	}
	if len(matches) != 1 {
		return agentLocation{}, fmt.Errorf("agent %q: %d matches (use --session to narrow, or the full id)", opts.Agent, len(matches))
	}
	return matches[0], nil
}

func sessionAgentMatches(transcriptPath, want string) []agentLocation {
	sessionDir := sessionDirFromPath(transcriptPath)
	head := scanSessionHead(transcriptPath)
	sessionID := filepath.Base(sessionDir)
	runDirs := append([]string{""}, transcript.WorkflowRunDirs(sessionDir)...)
	var matches []agentLocation
	for _, runDir := range runDirs {
		dir, runID := runDir, ""
		if runDir == "" {
			dir = filepath.Join(sessionDir, "subagents")
		} else {
			runID = filepath.Base(runDir)
		}
		for _, f := range transcript.WorkflowAgentFiles(dir) {
			if matchID(subagentIDFromPath(f), want) {
				matches = append(matches, agentLocation{
					SessionID: sessionID, TranscriptPath: f, SessionDir: sessionDir,
					RunDir: runDir, RunID: runID, Cwd: head.cwd,
				})
			}
		}
	}
	return matches
}

// matchID reports whether id equals want, or want is a non-empty prefix of it.
func matchID(id, want string) bool {
	return id == want || (want != "" && strings.HasPrefix(id, want))
}

// snapshotFrom builds one Snapshot from a resolved location, with the journal override for workflow agents.
func snapshotFrom(loc agentLocation, now time.Time, n int) Snapshot {
	meta := readSubagentMeta(loc.TranscriptPath)
	sub := buildSubagent(loc.TranscriptPath, now, loc.Cwd)

	var j journalEntry
	var found bool
	if loc.RunDir != "" {
		journal := readJournal(loc.RunDir)
		j, found = journal[sub.ID]
		sub = applyJournalEntry(sub, j)
	}

	result := ""
	switch {
	case found && j.Resulted:
		result = j.Result
	case sub.Status == StatusFinished && sub.Activity != nil:
		result = sub.Activity.Text
	}

	events := events(loc.TranscriptPath, loc.Cwd, n)
	if found && j.Resulted {
		events = append(events, Event{At: sub.Updated, Kind: "result", Size: j.ResultBytes})
	}

	return Snapshot{
		ID: sub.ID, SessionID: loc.SessionID, Label: sub.Description,
		Workflow: loc.RunID, Phase: transcript.WorkflowPhaseFromMeta(loc.TranscriptPath, io.Discard),
		Model: meta.Model, Effort: effortForLabel(loc, j.Label),
		Status: sub.Status, Updated: sub.Updated, Tokens: sub.Tokens,
		FilesEdited: editedFiles(loc.TranscriptPath), Dispatches: sub.Dispatches, Activity: sub.Activity,
		Result: result, Events: events,
	}
}

var effortLabelPattern = regexp.MustCompile(`label:\s*'([^']*)'`)
var effortValuePattern = regexp.MustCompile(`^[^}]*effort:\s*'([^']+)'`)

// effortForLabel extracts effort from the workflow script for the matching journal label.
func effortForLabel(loc agentLocation, label string) string {
	if loc.RunID == "" || label == "" {
		return ""
	}
	m := workflowManifests(loc.SessionDir)[loc.RunID]
	if m == nil {
		return ""
	}
	for _, match := range effortLabelPattern.FindAllStringSubmatchIndex(m.Script, -1) {
		if m.Script[match[2]:match[3]] == label {
			if effort := effortValuePattern.FindStringSubmatch(m.Script[match[1]:]); len(effort) == 2 {
				return effort[1]
			}
		}
	}
	return ""
}

// editedFiles lists the deduped file_path of every Edit/Write/MultiEdit tool_use in path, first-seen order.
func editedFiles(path string) []string {
	seen := map[string]bool{}
	var files []string
	eachLine(path, func(raw []byte) {
		var rec assistantContentLine
		if json.Unmarshal(raw, &rec) != nil || rec.Type != "assistant" {
			return
		}
		for _, c := range rec.Message.Content {
			if c.Type != "tool_use" || (c.Name != "Edit" && c.Name != "Write" && c.Name != "MultiEdit") {
				continue
			}
			fp := toolArg(c.Input)
			if fp != "" && !seen[fp] {
				seen[fp] = true
				files = append(files, fp)
			}
		}
	})
	return files
}

// events reads every tool_use/text event of path, oldest first, trimmed to the last n (n<=0 keeps all).
func events(path, cwd string, n int) []Event {
	home, _ := os.UserHomeDir()
	var all []Event
	eachLine(path, func(raw []byte) { all = append(all, eventsFromLine(raw, home, cwd)...) })
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// eventsFromLine turns one assistant transcript record into 0+ Events, one per tool_use/text block.
func eventsFromLine(raw []byte, home, cwd string) []Event {
	if !bytes.Contains(raw, toolUseMarker) && !bytes.Contains(raw, []byte(`"text"`)) {
		return nil
	}
	var rec assistantContentLine
	if json.Unmarshal(raw, &rec) != nil || rec.Type != "assistant" {
		return nil
	}
	at := parseRFC3339(rec.Timestamp)
	var out []Event
	for _, c := range rec.Message.Content {
		switch {
		case c.Type == "tool_use" && c.Name != "":
			out = append(out, Event{At: at, Kind: "tool", Tool: c.Name, Arg: StripPaths(toolArg(c.Input), home, cwd)})
		case c.Type == "text" && c.Text != "":
			out = append(out, Event{At: at, Kind: "text", Text: Truncate(c.Text, 80)})
		}
	}
	return out
}

// Follow resolves opts.Agent once, then polls its transcript, passing each new event to emit until the agent ends.
func Follow(ctx context.Context, opts ViewOptions, emit func(Event)) error {
	now := opts.now()
	loc, err := resolveAgent(opts.ClaudeDir, opts, now)
	if err != nil {
		return err
	}
	seen := len(events(loc.TranscriptPath, loc.Cwd, 0))
	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()
	drain := func() {
		seen = drainEvents(loc, seen, emit)
	}
	for {
		drain()
		if done, result := followDone(loc); done {
			drain() // events written between the read and the terminal check
			if result != nil {
				emit(*result)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func drainEvents(loc agentLocation, seen int, emit func(Event)) int {
	all := events(loc.TranscriptPath, loc.Cwd, 0)
	if len(all) == 0 && seen > 0 {
		return seen // an empty read after events is a failed open, not a rewrite: keep seen and retry
	}
	if len(all) < seen {
		seen = 0 // the transcript was truncated or rewritten
	}
	for _, e := range all[seen:] {
		emit(e)
	}
	return len(all)
}

// followDone reports a terminal state: a journal result line (workflow agent) or a finished meta (plain subagent).
func followDone(loc agentLocation) (bool, *Event) {
	if loc.RunDir != "" {
		id := subagentIDFromPath(loc.TranscriptPath)
		j, ok := readJournal(loc.RunDir)[id]
		if !ok || !j.Resulted {
			return false, nil
		}
		return true, &Event{At: time.Now(), Kind: "result", Size: j.ResultBytes}
	}
	return readSubagentMeta(loc.TranscriptPath).Finished, nil
}

// FormatEvent renders one Event as an "HH:MM:SS <kind> ..." line.
func FormatEvent(e Event) string {
	ts := e.At.Format("15:04:05")
	switch e.Kind {
	case "tool":
		return fmt.Sprintf("%s tool %s %s", ts, e.Tool, e.Arg)
	case "text":
		return fmt.Sprintf("%s text %q", ts, e.Text)
	case "result":
		return fmt.Sprintf("%s result %s", ts, compactBytes(e.Size))
	default:
		return ts
	}
}

// compactBytes renders n bytes as "12.3 KB" / "1.2 MB" style, plain bytes below 1KB.
func compactBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
