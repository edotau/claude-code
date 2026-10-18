package agenttree

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
)

// harvestTitlePrefix is the background-harvest system prompt, never a real user ask.
const harvestTitlePrefix = "Background harvest of a finished session"

// isSkippableTitle rejects a candidate title that isn't a real user prompt: a tag-wrapped harness
// directive (<local-command-caveat>, <command-name>, ...) or the SessionEnd harvest's own prompt.
func isSkippableTitle(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, harvestTitlePrefix)
}

// headScanBytes bounds the main-transcript head read: cwd/branch/title all appear in the first
// handful of records, long before a transcript's bulk (tool output, hook attachments) accumulates.
const headScanBytes = 64 << 10

// tailScanBytes bounds the subagent tail read for the last tool name, mirroring transcript.LastUsage.
const tailScanBytes = 256 << 10

// subagentMeta is the subset of agent-<id>.meta.json this view renders.
type subagentMeta struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	SpawnDepth  int    `json:"spawnDepth"`
	Finished    bool   `json:"finished"`
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
type workflowManifest struct {
	RunID     string `json:"runId"`
	Timestamp string `json:"timestamp"`
	Script    string `json:"script"`
	Result    struct {
		Status string `json:"status"`
	} `json:"result"`
	Summary string `json:"summary"`
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

// userTurnText reads a user record's message.content, which is either a plain string or an array
// of content blocks (the first text block wins; tool_result-only turns yield "").
func userTurnText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// truncate renders s on one line, cut to n runes with an ellipsis when longer.
func truncate(s string, n int) string {
	s = oneLine(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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

// lastToolName scans a subagent transcript's tail for the last assistant tool_use block's name.
func lastToolName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := int64(0)
	if fi.Size() > tailScanBytes {
		off = fi.Size() - tailScanBytes
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}

	name := ""
	for _, line := range bytes.Split(buf, []byte("\n")) {
		if !bytes.Contains(line, toolUseMarker) {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" {
			continue
		}
		for _, c := range rec.Message.Content {
			if c.Type == "tool_use" && c.Name != "" {
				name = c.Name
			}
		}
	}
	return name
}
