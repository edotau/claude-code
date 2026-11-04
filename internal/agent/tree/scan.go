package tree

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// updateTitlePrefix is the background-update system prompt, never a real user ask.
const updateTitlePrefix = "Background update of a finished session"

// isSkippableTitle rejects a tag-wrapped harness directive or the SessionEnd update prompt as a title.
func isSkippableTitle(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, updateTitlePrefix)
}

// headScanBytes bounds the main-transcript head read: cwd/branch/title appear in the first few records.
const headScanBytes = 64 << 10

// activityScanBytes bounds the subagent tail read for the newest tool/text activity signal.
const activityScanBytes = 64 << 10

// grepScanMaxLine bounds one scanned line of the full-file greps (dispatch ids, edited files, events, journal).
const grepScanMaxLine = 10 << 20

// subagentMeta is the subset of agent-<id>.meta.json this view renders.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	SpawnDepth  int    `json:"spawnDepth"`
	Finished    bool   `json:"finished"`
	Model       string `json:"model"`
}

// readSubagentMeta reads the sidecar .meta.json beside transcriptPath; a missing or bad one yields a zero meta.
func readSubagentMeta(transcriptPath string) subagentMeta {
	raw, err := os.ReadFile(strings.TrimSuffix(transcriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return subagentMeta{}
	}
	var m subagentMeta
	_ = json.Unmarshal(raw, &m)
	return m
}

// workflowManifest is a workflows/wf_<id>.json run record; Result stays RawMessage (string, object or null).
type workflowManifest struct {
	RunID     string          `json:"runId"`
	Timestamp string          `json:"timestamp"`
	Script    string          `json:"script"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"` // the run's own completed|killed, present on every manifest
}

// resultStatus reads result.status, else the manifest status (completed -> finished, killed -> error); "" otherwise.
func (m *workflowManifest) resultStatus() string {
	var r struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(m.Result, &r) == nil && r.Status != "" {
		return r.Status
	}
	switch m.Status {
	case "killed":
		return StatusError
	case "completed":
		return StatusFinished
	}
	return ""
}

// manifestNamePattern pulls the workflow's declared name out of its script's `meta = { name: '...' }`.
var manifestNamePattern = regexp.MustCompile(`name:\s*'([^']+)'`)

// name resolves the manifest's display name from its script's meta.name.
func (m *workflowManifest) name() string {
	if match := manifestNamePattern.FindStringSubmatch(m.Script); len(match) == 2 {
		return match[1]
	}
	return ""
}

// workflowManifests reads every <sessionDir>/workflows/wf_*.json manifest, keyed by runId.
func workflowManifests(sessionDir string) map[string]*workflowManifest {
	out := map[string]*workflowManifest{}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "workflows", "wf_*.json"))
	if err != nil {
		return out
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m workflowManifest
		if json.Unmarshal(raw, &m) != nil || m.RunID == "" {
			continue
		}
		out[m.RunID] = &m
	}
	return out
}

// journalEntry records lifecycle state and a result preview; a null result marks a killed agent.
type journalEntry struct {
	Label       string
	Started     bool
	Resulted    bool
	Errored     bool
	Result      string
	ResultBytes int
}

// journalLine is one journal.jsonl record; Result stays RawMessage so a JSON null differs from an absent field.
type journalLine struct {
	Type    string          `json:"type"`
	AgentID string          `json:"agentId"`
	Label   string          `json:"label"`
	Result  json.RawMessage `json:"result"`
}

// readJournal reads a run's journal.jsonl into one entry per agentId; a missing or bad journal gives an empty map.
func readJournal(runDir string) map[string]journalEntry {
	out := map[string]journalEntry{}
	eachLine(filepath.Join(runDir, "journal.jsonl"), func(raw []byte) {
		var line journalLine
		if json.Unmarshal(raw, &line) != nil || line.AgentID == "" {
			return
		}
		e := out[line.AgentID]
		switch line.Type {
		case "started":
			e.Started, e.Label = true, line.Label
		case "result":
			e.Resulted = true
			if isJSONNull(line.Result) {
				e.Errored = true
			} else {
				e.Result = resultPreview(line.Result)
			}
			e.ResultBytes = len(bytes.TrimSpace(line.Result))
		}
		out[line.AgentID] = e
	})
	return out
}

// eachLine calls fn on every line of path; an open failure or a too-long/truncated line ends the scan quietly.
func eachLine(path string, fn func(line []byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), grepScanMaxLine)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}

// isJSONNull reports whether raw is the literal JSON null (vs. absent/empty, which is "").
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// resultPreview renders a journal result value (string or object) as a short, one-line preview.
func resultPreview(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return Truncate(s, 200)
	}
	return Truncate(string(raw), 200)
}

// sessionHead is what the first ~64KB of a main transcript carries about the session itself.
type sessionHead struct {
	cwd, branch, title string
}

// sessionHeadLine is the head-record subset; cwd/gitBranch may ride on any line type, so the first carrier wins.
type sessionHeadLine struct {
	Type        string          `json:"type"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

// newLineScanner scans r by line with a 64KB initial and 1MB max buffer.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	return sc
}

// scanSessionHead streams the first headScanBytes of path for cwd, branch and the first human user turn as title.
func scanSessionHead(path string) sessionHead {
	var head sessionHead
	f, err := os.Open(path)
	if err != nil {
		return head
	}
	defer f.Close()

	sc := newLineScanner(io.LimitReader(f, headScanBytes))
	for sc.Scan() {
		var rec sessionHeadLine
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		head.cwd = cmp.Or(head.cwd, rec.Cwd)
		head.branch = cmp.Or(head.branch, rec.GitBranch)
		if head.title == "" {
			head.title = titleFromRecord(rec)
		}
		if head.cwd != "" && head.branch != "" && head.title != "" {
			break
		}
	}
	return head
}

// titleFromRecord returns the truncated human user-turn text of rec, "" when rec is not a usable title source.
func titleFromRecord(rec sessionHeadLine) string {
	if rec.Type != "user" || rec.IsSidechain {
		return ""
	}
	text := strings.TrimSpace(userTurnText(rec.Message))
	if text == "" || isSkippableTitle(text) {
		return ""
	}
	return Truncate(text, 60)
}

// userTurnText reads a user record's message.content: a plain string or an array of blocks (text parts concatenated).
func userTurnText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	return transcript.ContentText(msg.Content)
}

// Truncate renders s on one line, cut to n runes with an ellipsis when longer.
func Truncate(s string, n int) string {
	return textutil.CapRunes(oneLine(s), n, "…")
}

// oneLine collapses embedded newlines/tabs and repeated spaces so a multi-line prompt renders as
// one title line.
func oneLine(s string) string {
	out := make([]rune, 0, len(s))
	prevSpace := false
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		if c == ' ' && prevSpace {
			continue
		}
		prevSpace = c == ' '
		out = append(out, c)
	}
	return string(out)
}

var toolUseMarker = []byte(`"tool_use"`)

// assistantContentLine is the assistant record subset this file reads: tool_use/text blocks plus the timestamp.
type assistantContentLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// parseRFC3339 parses a transcript record's timestamp, zero on failure.
func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// tailBuf reads path's last n bytes (the whole file when smaller), nil on any read failure.
func tailBuf(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-n, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	return buf
}

// lastActivity tail-scans path for the newest assistant tool_use (Tool+Arg) and text block, paths shortened.
func lastActivity(path, cwd string) Activity {
	buf := tailBuf(path, activityScanBytes)
	if buf == nil {
		return Activity{}
	}
	home, _ := os.UserHomeDir()
	var act Activity
	for raw := range bytes.SplitSeq(buf, []byte("\n")) {
		for _, e := range eventsFromLine(raw, home, cwd) {
			if e.Kind == "tool" {
				act.Tool, act.Arg, act.At = e.Tool, e.Arg, e.At
			} else {
				act.Text, act.At = e.Text, e.At
			}
		}
	}
	return act
}

// toolArg extracts the one input field most useful to show, in priority order: file_path, path,
// command (truncated to 60 runes, for Bash), pattern (Grep/Glob).
func toolArg(input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
		Pattern  string `json:"pattern"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	switch {
	case in.FilePath != "":
		return in.FilePath
	case in.Path != "":
		return in.Path
	case in.Command != "":
		return Truncate(in.Command, 60)
	default:
		return in.Pattern
	}
}

// StripPaths removes the session cwd and collapses $HOME to "~" wherever either appears in s.
func StripPaths(s, home, cwd string) string {
	if cwd != "" {
		s = strings.ReplaceAll(s, cwd+"/", "")
		s = strings.ReplaceAll(s, cwd, "")
	}
	if home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// pendingAskMarker flags a Bash arg as a relay that launched `ask` and has not seen its exit line yet.
const pendingAskMarker = "ask --agent"

// firstTimestamp reads a transcript first record timestamp: the subagent start time, zero on failure.
func firstTimestamp(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := newLineScanner(f)
	if sc.Scan() {
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			return parseRFC3339(rec.Timestamp)
		}
	}
	return time.Time{}
}

// dispatchFromRecord renders a dispatch record; a still-running record reports its elapsed time so far.
func dispatchFromRecord(rec agent.Record, matched string, now time.Time) Dispatch {
	duration := rec.Elapsed()
	if duration == "" && rec.Status == "running" {
		duration = now.Sub(rec.Time).Round(time.Second).String()
	}
	return Dispatch{
		ID: rec.ID, Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model,
		Status: rec.Status, Duration: duration, Fallback: rec.Fallback,
		FallbackReason: rec.FallbackReason, Matched: matched,
	}
}

// buildDispatches returns each "ask: dispatch <id>" the transcript shows, plus a heuristic match when `ask` is pending.
func buildDispatches(transcriptPath, cwd string, now time.Time, act Activity) []Dispatch {
	var dispatches []Dispatch
	matched := map[string]bool{}
	for _, id := range transcript.DispatchIDs(transcriptPath) {
		rec, err := agent.LoadRecord(id)
		if err != nil {
			continue
		}
		dispatches = append(dispatches, dispatchFromRecord(rec, "", now))
		matched[rec.ID] = true
	}
	if act.Tool == "Bash" && strings.Contains(act.Arg, pendingAskMarker) {
		if disp := pendingDispatch(transcriptPath, cwd, matched, now); disp != nil {
			dispatches = append(dispatches, *disp)
		}
	}
	return dispatches
}

// pendingDispatch matches the newest running dispatch for this cwd that started no earlier than the subagent.
func pendingDispatch(transcriptPath, cwd string, matched map[string]bool, now time.Time) *Dispatch {
	start := firstTimestamp(transcriptPath)
	for _, rec := range agent.RecentRecords(50) {
		if matched[rec.ID] || rec.Status != "running" || rec.Request.WorkDir != cwd {
			continue
		}
		if !start.IsZero() && rec.Time.Before(start) {
			continue
		}
		disp := dispatchFromRecord(rec, "heuristic", now)
		return &disp
	}
	return nil
}
