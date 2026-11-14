package transcript

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// ContextRow is one source's share of the context a session transcript logged.
type ContextRow struct {
	Source  string  `json:"source"`
	Count   int     `json:"count"`
	Bytes   int64   `json:"bytes"`
	Tokens  int64   `json:"tokens"`
	Percent float64 `json:"percent"`
}

// ContextEntry is one logged item (a tool result, a hook injection): its source label and transcript line, no text.
type ContextEntry struct {
	Source string `json:"source"`
	Line   int    `json:"line"`
	Bytes  int64  `json:"bytes"`
	Tokens int64  `json:"tokens"`
}

// ContextReport is the per-source table for one transcript, largest row first.
type ContextReport struct {
	Rows        []ContextRow   `json:"rows"`
	Top         []ContextEntry `json:"top"`
	TotalBytes  int64          `json:"total_bytes"`
	TotalTokens int64          `json:"total_tokens"`
}

const topEntries = 5

// EstimateTokens is the table's token rule: UTF-8 bytes divided by 4, rounded up (code and JSON run nearer 3 bytes).
func EstimateTokens(bytes int64) int64 { return (bytes + 3) / 4 }

type ctxPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

type ctxLine struct {
	Type             string `json:"type"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Message          struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Rendered []struct {
		Content string `json:"content"`
	} `json:"rendered"`
	Attachment struct {
		Type     string          `json:"type"`
		HookName string          `json:"hookName"`
		Prompt   json.RawMessage `json:"prompt"`
	} `json:"attachment"`
}

type contextCounter struct {
	rows  map[string]*ContextRow
	top   []ContextEntry
	tools map[string]string // tool_use id -> tool name
}

// ContextBySource totals what one session transcript logged as context, by source; main chain only, bytes only.
func ContextBySource(path string) (*ContextReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &contextCounter{rows: map[string]*ContextRow{}, tools: map[string]string{}}
	n := 0
	if err := EachLine(f, func(raw []byte) { n++; c.addLine(n, raw) }); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return c.report(), nil
}

// SessionFile is the main transcript of session, or the newest one of cwd's project when session is "".
func SessionFile(cwd, session string) (string, error) {
	if session == "" {
		p, err := defaultSource(cwd)
		if err != nil {
			return "", fmt.Errorf("%w (or pass --session)", err)
		}
		return p, nil
	}
	matches, err := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", session+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("no transcript for session %q in any project", session)
	}
	slices.Sort(matches)
	return matches[0], nil
}

func (c *contextCounter) addLine(n int, raw []byte) {
	var l ctxLine
	if json.Unmarshal(raw, &l) != nil || l.IsSidechain {
		return
	}
	switch l.Type {
	case "attachment":
		size := 0
		for _, r := range l.Rendered {
			size += len(r.Content)
		}
		c.add(attachmentSource(&l), size, n)
	case "user":
		c.addUser(&l, n)
	case "assistant":
		c.addAssistant(&l, n)
	}
}

func (c *contextCounter) addUser(l *ctxLine, n int) {
	source := "user text"
	switch {
	case l.IsCompactSummary:
		source = "compaction summary"
	case l.IsMeta:
		source = "injected user message"
	}
	var parts []ctxPart
	if json.Unmarshal(l.Message.Content, &parts) != nil { // a bare string
		text := ContentText(l.Message.Content)
		c.add(cmp.Or(userKind(l, text), source), len(text), n)
		return
	}
	for _, p := range parts {
		switch p.Type {
		case "tool_result":
			c.add("tool result: "+c.toolName(p.ToolUseID), len(ContentText(p.Content)), n)
		case "text":
			c.add(cmp.Or(userKind(l, p.Text), source), len(p.Text), n)
		}
	}
}

// userKind is the hand-back or notification kind of a meta user message, "" for any other.
func userKind(l *ctxLine, text string) string {
	if !l.IsMeta {
		return ""
	}
	return messageKind(text)
}

func (c *contextCounter) addAssistant(l *ctxLine, n int) {
	var parts []ctxPart
	if json.Unmarshal(l.Message.Content, &parts) != nil {
		return
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			c.add("assistant text", len(p.Text), n)
		case "thinking":
			c.add("assistant thinking", len(p.Thinking), n)
		case "tool_use":
			c.tools[p.ID] = p.Name
			c.add("tool call: "+p.Name, len(p.Input), n)
		}
	}
}

func (c *contextCounter) toolName(id string) string {
	if name := c.tools[id]; name != "" {
		return name
	}
	return "unknown"
}

// add counts one item of size bytes under source and keeps the topEntries largest items.
func (c *contextCounter) add(source string, size, line int) {
	if size <= 0 {
		return
	}
	r := c.rows[source]
	if r == nil {
		r = &ContextRow{Source: source}
		c.rows[source] = r
	}
	r.Count++
	r.Bytes += int64(size)
	e := ContextEntry{Source: source, Line: line, Bytes: int64(size)}
	if len(c.top) == topEntries && e.Bytes <= c.top[topEntries-1].Bytes {
		return
	}
	c.top = append(c.top, e)
	slices.SortStableFunc(c.top, func(a, b ContextEntry) int { return cmp.Compare(b.Bytes, a.Bytes) })
	c.top = c.top[:min(len(c.top), topEntries)]
}

func (c *contextCounter) report() *ContextReport {
	rep := &ContextReport{Rows: make([]ContextRow, 0, len(c.rows)), Top: c.top}
	for _, r := range c.rows {
		rep.TotalBytes += r.Bytes
	}
	for _, r := range c.rows {
		r.Tokens = EstimateTokens(r.Bytes)
		r.Percent = float64(r.Bytes) * 100 / float64(rep.TotalBytes)
		rep.Rows = append(rep.Rows, *r)
	}
	slices.SortFunc(rep.Rows, func(a, b ContextRow) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), strings.Compare(a.Source, b.Source))
	})
	for i := range rep.Top {
		rep.Top[i].Tokens = EstimateTokens(rep.Top[i].Bytes)
	}
	rep.TotalTokens = EstimateTokens(rep.TotalBytes)
	return rep
}

// reminderSources groups attachment types that carry one kind of context; any other type is "reminder: <type>".
var reminderSources = map[string]string{
	"instructions": "rules and CLAUDE.md", "nested_memory": "rules and CLAUDE.md",
	"skill_listing": "system prompt additions", "output_style_instructions": "system prompt additions",
	"mcp_instructions_delta": "system prompt additions", "deferred_tools_delta": "system prompt additions",
	"agent_listing_delta": "system prompt additions", "session_context": "system prompt additions",
	"model": "system prompt additions", "date": "system prompt additions",
	"invoked_skills": "file and skill re-reads", "file": "file and skill re-reads",
	"compact_file_reference": "file and skill re-reads", "edited_text_file": "file and skill re-reads",
}

// hookKinds label a hook's injection by the start of its payload; the payload itself is never printed.
var hookKinds = []string{
	"memory-recall", "[prompt-coach]", "Paths:", "Peer session", "review-simplicity", "ide_diagnostics",
}

func attachmentSource(l *ctxLine) string {
	a := l.Attachment
	switch a.Type {
	case "queued_command":
		return cmp.Or(messageKind(ContentText(a.Prompt)), "queued messages")
	case "hook_additional_context":
		return "hook " + a.HookName + hookKind(l)
	}
	if s, ok := reminderSources[a.Type]; ok {
		return s
	}
	return "reminder: " + a.Type
}

// messageKind names a delivered message by its opening tag within the first 80 bytes, "" when it is neither kind.
func messageKind(text string) string {
	head := text[:min(len(text), 80)]
	switch {
	case strings.Contains(head, "<agent-message"):
		return "subagent hand-backs"
	case strings.Contains(head, "<task-notification"):
		return "task notifications"
	}
	return ""
}

// hookKind is " <kind>" when the first 40 bytes after the hook's "context: " lead-in match a known kind, else "".
func hookKind(l *ctxLine) string {
	if len(l.Rendered) == 0 {
		return ""
	}
	_, payload, _ := strings.Cut(l.Rendered[0].Content, "context: ")
	payload = payload[:min(len(payload), 40)]
	for _, k := range hookKinds {
		if strings.Contains(payload, k) {
			return " " + strings.Trim(k, "[]:")
		}
	}
	return ""
}
