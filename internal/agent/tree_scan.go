package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// updateTitlePrefix is the background-update system prompt, never a real user ask.
const updateTitlePrefix = "Background update of a finished session"

// isSkippableTitle rejects a candidate title that isn't a real user prompt: a tag-wrapped harness
// directive (<local-command-caveat>, <command-name>, ...) or the SessionEnd update's own prompt.
func isSkippableTitle(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, updateTitlePrefix)
}

// headScanBytes bounds the main-transcript head read: cwd/branch/title all appear in the first
// handful of records, long before a transcript's bulk (tool output, hook attachments) accumulates.
const headScanBytes = 64 << 10

// activityScanBytes bounds the subagent tail read for the newest tool/text activity signal.
const activityScanBytes = 64 << 10

// grepScanMaxLine bounds a single scanned line for the full-file greps below (dispatch ids, edited
// files): generous enough for a large tool_result without unbounded memory on a pathological line.
const grepScanMaxLine = 10 << 20

// subagentMeta is the subset of agent-<id>.meta.json this view renders.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	SpawnDepth  int    `json:"spawnDepth"`
	Finished    bool   `json:"finished"`
	Model       string `json:"model"`
}

// readSubagentMeta reads the sidecar .meta.json beside transcriptPath; a missing or unparsable
// sidecar degrades to a zero-value meta rather than failing the whole scan.
func readSubagentMeta(transcriptPath string) subagentMeta {
	raw, err := os.ReadFile(trimJSONLSuffix(transcriptPath) + ".meta.json")
	if err != nil {
		return subagentMeta{}
	}
	var m subagentMeta
	_ = json.Unmarshal(raw, &m)
	return m
}

func trimJSONLSuffix(s string) string {
	const suffix = ".jsonl"
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

// workflowManifest is a workflows/wf_<id>.json run record — the Workflow tool's own summary,
// distinct from (but runId-linked to) the subagents/workflows/<runId>/agent-*.jsonl transcripts.
// Result is whatever the script's own `return` statement produced: a string, an object, or null
// (a killed run) — RawMessage so none of those shapes ever fails the manifest's own decode.
type workflowManifest struct {
	RunID     string          `json:"runId"`
	Timestamp string          `json:"timestamp"`
	Script    string          `json:"script"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"` // the run's own completed|killed, present on every manifest
}

// resultStatus reads result.status when the script's return value was an object carrying one (the
// convention green-keeper-loop-style workflows use for their own green/red outcome), falling back to the
// manifest's own top-level status (completed -> finished, killed -> error); "" otherwise.
func (m *workflowManifest) resultStatus() string {
	var r struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(m.Result, &r) == nil && r.Status != "" {
		return r.Status
	}
	switch m.Status {
	case "killed":
		return statusError
	case "completed":
		return statusFinished
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

// timestamp parses the manifest's own RFC3339 timestamp, zero on failure.
func (m *workflowManifest) timestamp() time.Time {
	t, err := time.Parse(time.RFC3339, m.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
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

// journalLine is one journal.jsonl record; Result stays RawMessage so a JSON null is distinguishable
// from an absent field and a string/object result never fails this line's own decode.
type journalLine struct {
	Type    string          `json:"type"`
	AgentID string          `json:"agentId"`
	Label   string          `json:"label"`
	Result  json.RawMessage `json:"result"`
}

// readJournal reads one workflow run's journal.jsonl into one entry per agentId; a missing or
// unparsable journal (an older run, or one still only at "launched") degrades to an empty map.
func readJournal(runDir string) map[string]journalEntry {
	out := map[string]journalEntry{}
	f, err := os.Open(filepath.Join(runDir, "journal.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), grepScanMaxLine)
	for sc.Scan() {
		var line journalLine
		if json.Unmarshal(sc.Bytes(), &line) != nil || line.AgentID == "" {
			continue
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
	}
	_ = sc.Err() // a too-long/truncated line mid-scan degrades to the entries already collected
	return out
}

// isJSONNull reports whether raw is the literal JSON null (vs. absent/empty, which is "").
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// resultPreview renders a journal result value (string or object) as a short, one-line preview.
func resultPreview(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return truncate(s, 200)
	}
	return truncate(string(raw), 200)
}

// sessionHead is what the first ~64KB of a main transcript carries about the session itself.
type sessionHead struct {
	cwd, branch, title string
}

// sessionHeadLine is the subset of fields a head record may carry; cwd/gitBranch ride on hook
// attachment/sidecar lines too (a session's first lines are "last-prompt"/"mode" markers with
// neither field), so the first line to carry either wins regardless of its own "type".
type sessionHeadLine struct {
	Type        string          `json:"type"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

// scanSessionHead streams the first headScanBytes of path, collecting cwd/branch from the first
// record that carries them and a title from the first non-sidechain human "user" turn.
func scanSessionHead(path string) sessionHead {
	var head sessionHead
	f, err := os.Open(path)
	if err != nil {
		return head
	}
	defer f.Close()

	sc := bufio.NewScanner(io.LimitReader(f, headScanBytes))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var rec sessionHeadLine
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if head.cwd == "" && rec.Cwd != "" {
			head.cwd = rec.Cwd
		}
		if head.branch == "" && rec.GitBranch != "" {
			head.branch = rec.GitBranch
		}
		if head.title == "" && rec.Type == "user" && !rec.IsSidechain {
			if text := strings.TrimSpace(userTurnText(rec.Message)); text != "" && !isSkippableTitle(text) {
				head.title = truncate(text, 60)
			}
		}
		if head.cwd != "" && head.branch != "" && head.title != "" {
			break
		}
	}
	return head
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

// truncate renders s on one line, cut to n runes with an ellipsis when longer.
func truncate(s string, n int) string {
	s = oneLine(s)
	if len([]rune(s)) <= n {
		return s
	}
	return textutil.TruncateRunes(s, n) + "…"
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

// assistantContentLine is the subset of one assistant transcript record this file reads repeatedly:
// its tool_use/text content blocks plus the record's own timestamp.
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

// tailBuf reads path's last n bytes (the whole file when smaller), "" on any read failure.
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
	off := int64(0)
	if fi.Size() > n {
		off = fi.Size() - n
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	return buf
}

// lastActivity tail-scans path's last ~64KB for the newest assistant tool_use (Tool+Arg+At) and the
// newest assistant text block (Text); Arg has $HOME and cwd stripped so a path reads short.
func lastActivity(path, cwd string) Activity {
	buf := tailBuf(path, activityScanBytes)
	if buf == nil {
		return Activity{}
	}
	home, _ := os.UserHomeDir()
	var act Activity
	for raw := range bytes.SplitSeq(buf, []byte("\n")) {
		if !bytes.Contains(raw, toolUseMarker) && !bytes.Contains(raw, []byte(`"text"`)) {
			continue
		}
		var rec assistantContentLine
		if json.Unmarshal(raw, &rec) != nil || rec.Type != "assistant" {
			continue
		}
		at := parseRFC3339(rec.Timestamp)
		for _, c := range rec.Message.Content {
			switch {
			case c.Type == "tool_use" && c.Name != "":
				act.Tool, act.Arg, act.At = c.Name, stripPaths(toolArg(c.Input), home, cwd), at
			case c.Type == "text" && c.Text != "":
				act.Text, act.At = truncate(c.Text, 80), at
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
		return truncate(in.Command, 60)
	default:
		return in.Pattern
	}
}

// stripPaths removes the session cwd (the more specific, usually longer prefix) and collapses $HOME
// to "~" wherever either appears in s — a label or tool arg is often a path, but can also be prose
// mentioning one mid-sentence.
func stripPaths(s, home, cwd string) string {
	if cwd != "" {
		s = strings.ReplaceAll(s, cwd+"/", "")
		s = strings.ReplaceAll(s, cwd, "")
	}
	if home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

// dispatchIDPattern matches the `ask` CLI's own exit line, "ask: dispatch <id> (...)", wherever it
// shows up inside a relay subagent's tool_result content.
var dispatchIDPattern = regexp.MustCompile(`ask: dispatch (\S+)`)

// dispatchIDs greps path for every "ask: dispatch <id>" line, deduped, in first-seen order.
func dispatchIDs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), grepScanMaxLine)
	seen := map[string]bool{}
	var ids []string
	for sc.Scan() {
		for _, m := range dispatchIDPattern.FindAllStringSubmatch(sc.Text(), -1) {
			if id := m[1]; !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	_ = sc.Err() // a too-long/truncated line mid-scan degrades to the ids already collected
	return ids
}

// pendingAskMarker flags a Bash activity arg as "this relay just launched `ask` and hasn't seen its
// exit line yet" — the signal for the heuristic running-leg match below.
const pendingAskMarker = "ask --agent"

// firstTimestamp reads a transcript's first record's own timestamp — a subagent's start time, used
// to bound the heuristic leg match to dispatches that began no earlier than this subagent did.
func firstTimestamp(path string) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if sc.Scan() {
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			return parseRFC3339(rec.Timestamp)
		}
	}
	_ = sc.Err() // no first line (empty/unreadable file) degrades to the zero time
	return time.Time{}
}

// legFromRecord renders one dispatch record as a TreeLeg; a still-running record has no final Duration
// yet, so this reports the elapsed time so far instead.
func legFromRecord(rec Record, matched string, now time.Time) TreeLeg {
	duration := rec.Elapsed()
	if duration == "" && rec.Status == "running" {
		duration = now.Sub(rec.Time).Round(time.Second).String()
	}
	return TreeLeg{
		ID: rec.ID, Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model,
		Status: rec.Status, Duration: duration, Fallback: rec.Fallback,
		FallbackReason: rec.FallbackReason, Matched: matched,
	}
}

// buildLegs attaches one TreeLeg per agent dispatch this subagent's transcript shows: every "ask:
// dispatch <id>" exit line it has already seen, plus (when its newest activity is a Bash call still
// waiting on `ask`) a heuristic match against the dispatch log's own still-running records.
func buildLegs(transcriptPath, cwd string, now time.Time, act Activity) []TreeLeg {
	var legs []TreeLeg
	matched := map[string]bool{}
	for _, id := range dispatchIDs(transcriptPath) {
		rec, err := LoadRecord(id)
		if err != nil {
			continue
		}
		legs = append(legs, legFromRecord(rec, "", now))
		matched[rec.ID] = true
	}
	if act.Tool == "Bash" && strings.Contains(act.Arg, pendingAskMarker) {
		if leg := pendingLeg(transcriptPath, cwd, matched, now); leg != nil {
			legs = append(legs, *leg)
		}
	}
	return legs
}

// pendingLeg matches a leg with no exit line yet: the newest still-running dispatch for this
// subagent's own cwd that started no earlier than the subagent itself did.
func pendingLeg(transcriptPath, cwd string, matched map[string]bool, now time.Time) *TreeLeg {
	start := firstTimestamp(transcriptPath)
	for _, rec := range RecentRecords(50) {
		if matched[rec.ID] || rec.Status != "running" || rec.Request.WorkDir != cwd {
			continue
		}
		if !start.IsZero() && rec.Time.Before(start) {
			continue
		}
		leg := legFromRecord(rec, "heuristic", now)
		return &leg
	}
	return nil
}
