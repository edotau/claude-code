package tree

import (
	"bufio"
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

// followPollInterval is how often Follow re-reads the transcript and the journal/meta looking for
// new events and the terminal result.
var followPollInterval = 500 * time.Millisecond

// ViewOptions scopes View/Follow: ClaudeDir + the same Session/Days scoping as Options, narrowed to
// one Agent (its full id, or a prefix unique across the sessions in scope).
type ViewOptions struct {
	ClaudeDir string
	Session   string
	Days      int
	Agent     string
	N         int // trailing events kept in Snapshot.Events; <=0 -> defaultEventCount
	Now       func() time.Time
}

func (o ViewOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Event is one line in a subagent's event stream: a tool call, an assistant text blurb, or the
// terminal journal result.
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
	Dispatches  []Dispatch `json:"legs,omitempty"`
	Activity    *Activity  `json:"activity,omitempty"`
	Result      string     `json:"result,omitempty"`
	Events      []Event    `json:"events,omitempty"`
}

// agentLocation is where one resolved agent's transcript lives: its session, and the workflow run
// dir when it's inside one ("" for a plain dispatched subagent).
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

// resolveAgent searches every session in scope — its own subagents/ and every subagents/workflows/*/
// — for exactly one agent id matching opts.Agent exactly or as a unique prefix.
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
		sessionDir := strings.TrimSuffix(transcriptPath, ".jsonl")
		head := scanSessionHead(transcriptPath)
		sessionID := filepath.Base(sessionDir)
		for _, f := range subagentFiles(sessionDir) {
			if matchID(subagentIDFromPath(f), opts.Agent) {
				matches = append(matches, agentLocation{SessionID: sessionID, TranscriptPath: f, SessionDir: sessionDir, Cwd: head.cwd})
			}
		}
		for _, runDir := range transcript.WorkflowRunDirs(sessionDir) {
			for _, f := range transcript.WorkflowAgentFiles(runDir) {
				if matchID(subagentIDFromPath(f), opts.Agent) {
					matches = append(matches, agentLocation{
						SessionID: sessionID, TranscriptPath: f, SessionDir: sessionDir,
						RunDir: runDir, RunID: filepath.Base(runDir), Cwd: head.cwd,
					})
				}
			}
		}
	}
	if len(matches) != 1 {
		return agentLocation{}, fmt.Errorf("agent %q: %d matches (use --session to narrow, or the full id)", opts.Agent, len(matches))
	}
	return matches[0], nil
}

// matchID reports whether id equals want, or want is a non-empty prefix of it.
func matchID(id, want string) bool {
	return id == want || (want != "" && strings.HasPrefix(id, want))
}

// snapshotFrom builds one Snapshot from a resolved location, applying the same journal override
// buildOneWorkflowRun does for a workflow agent.
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
	case sub.Status == statusFinished && sub.Activity != nil:
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

// editedFiles lists the file_path arguments of every Edit/Write/MultiEdit tool_use in path, deduped
// in first-seen order.
func editedFiles(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), grepScanMaxLine)
	seen := map[string]bool{}
	var files []string
	for sc.Scan() {
		var rec assistantContentLine
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "assistant" {
			continue
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
	}
	_ = sc.Err() // a too-long/truncated line mid-scan degrades to the files already collected
	return files
}

// events reads every tool_use/text event out of path, oldest first, trimmed to the last n (n<=0
// keeps all of them — Follow's baseline count needs the full list).
func events(path, cwd string, n int) []Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	home, _ := os.UserHomeDir()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), grepScanMaxLine)
	var all []Event
	for sc.Scan() {
		all = append(all, eventsFromLine(sc.Bytes(), home, cwd)...)
	}
	_ = sc.Err() // a too-long/truncated line mid-scan degrades to the events already collected
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
			out = append(out, Event{At: at, Kind: "tool", Tool: c.Name, Arg: stripPaths(toolArg(c.Input), home, cwd)})
		case c.Type == "text" && c.Text != "":
			out = append(out, Event{At: at, Kind: "text", Text: truncate(c.Text, 80)})
		}
	}
	return out
}

// Follow resolves opts.Agent once, then polls its transcript every 500ms, writing one formatted line
// per new event to w, until the agent's result appears (its journal result line inside a workflow
// run, or its sidecar meta's "finished" flag otherwise) or ctx is cancelled.
func Follow(ctx context.Context, opts ViewOptions, w io.Writer) error {
	now := opts.now()
	loc, err := resolveAgent(opts.ClaudeDir, opts, now)
	if err != nil {
		return err
	}
	seen := len(events(loc.TranscriptPath, loc.Cwd, 0))
	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()
	for {
		all := events(loc.TranscriptPath, loc.Cwd, 0)
		if len(all) < seen {
			seen = 0
		}
		for _, e := range all[seen:] {
			fmt.Fprintln(w, formatEvent(e))
		}
		seen = len(all)
		if done, result := followDone(loc); done {
			if result != nil {
				fmt.Fprintln(w, formatEvent(*result))
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

// followDone reports whether the resolved agent has reached a terminal state: a workflow agent once
// its journal carries a result line (the result Event to emit, if any); a plain subagent once its
// sidecar meta reports "finished".
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

// formatEvent renders one Event as "HH:MM:SS tool Read path…" / "HH:MM:SS text "…"" / "HH:MM:SS
// result 12.3 KB".
func formatEvent(e Event) string {
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

// RenderSnapshot writes a Snapshot as the `--format text` drill-down view.
func RenderSnapshot(w io.Writer, snap Snapshot) {
	fmt.Fprintf(w, "%s  %s\n", snap.ID, snap.Label)
	if snap.Workflow != "" {
		fmt.Fprintf(w, "workflow %s  phase %s\n", snap.Workflow, snap.Phase)
	}
	model := snap.Model
	if snap.Effort != "" {
		model += " effort=" + snap.Effort
	}
	fmt.Fprintf(w, "status %s %s  model %s  tokens %s\n", statusGlyph(snap.Status), snap.Status, model, tokenCompact(snap.Tokens))
	if snap.Activity != nil {
		fmt.Fprintf(w, "last activity: %s %s (%s)\n", snap.Activity.Tool, snap.Activity.Arg, snap.Activity.At.Format(time.RFC3339))
	}
	for _, f := range snap.FilesEdited {
		fmt.Fprintf(w, "  edited %s\n", f)
	}
	for _, disp := range snap.Dispatches {
		fmt.Fprintf(w, "  %s\n", formatDispatch(disp))
	}
	for _, e := range snap.Events {
		fmt.Fprintln(w, formatEvent(e))
	}
	if snap.Result != "" {
		fmt.Fprintf(w, "result: %s\n", snap.Result)
	}
}
