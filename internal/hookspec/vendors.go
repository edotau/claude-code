package hookspec

import (
	"encoding/json"
	"slices"
	"strings"
)

var vendorEvents = map[string]string{
	EventPreToolUse:       "preToolUse",
	EventPostToolUse:      "postToolUse",
	EventUserPromptSubmit: "userPromptSubmitted",
}

// CodexJSON renders only explicitly allowed hooks in Codex's Claude-compatible schema.
func CodexJSON() ([]byte, error) {
	events := map[string][]MatcherGroup{}
	for _, s := range Specs() {
		if !slices.Contains(s.Vendors, "codex") {
			continue
		}
		h := Hook{Type: "command", Command: AgentEnv + "=codex " + s.Command(), Timeout: s.Seconds(), Async: s.Async, StatusMessage: s.StatusMessage}
		groups := events[s.Event]
		if n := len(groups); n > 0 && groups[n-1].Matcher == s.Matcher {
			groups[n-1].Hooks = append(groups[n-1].Hooks, h)
		} else {
			events[s.Event] = append(groups, MatcherGroup{Matcher: s.Matcher, Hooks: []Hook{h}})
		}
	}
	return json.MarshalIndent(map[string]any{"hooks": events}, "", "  ")
}

// CopilotJSON renders user-level hooks; advisory failures must never deny a tool.
func CopilotJSON() ([]byte, error) {
	type entry struct {
		Type       string `json:"type"`
		Matcher    string `json:"matcher"`
		Bash       string `json:"bash"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	events := map[string][]entry{}
	for _, s := range Specs() {
		event := vendorEvents[s.Event]
		if !slices.Contains(s.Vendors, "copilot") || event == "" {
			continue
		}
		command := s.Command()
		if !s.Blocks {
			command += " || true"
		}
		matcher := s.Matcher
		if matcher == "*" {
			matcher = ""
		} else if matcher != "" {
			matcher = strings.NewReplacer("Write", "Write|create", "Read", "Read|view").Replace(matcher)
			matcher = copilotMatcher(matcher)
		}
		events[event] = append(events[event], entry{Type: "command", Matcher: matcher, Bash: command, TimeoutSec: s.Seconds()})
	}
	return json.MarshalIndent(map[string]any{"version": 1, "hooks": events}, "", "  ")
}

// copilotMatcher lists each tool name as given and lowercased: Copilot rejects inline flags like (?i) and anchors.
func copilotMatcher(m string) string {
	var out []string
	seen := map[string]bool{}
	for n := range strings.SplitSeq(m, "|") {
		for _, v := range []string{n, strings.ToLower(n)} {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, "|")
}
