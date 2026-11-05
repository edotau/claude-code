package copilot

import (
	"encoding/json"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

var agentEvents = map[string]string{
	settings.EventPreToolUse:       "preToolUse",
	settings.EventPostToolUse:      "postToolUse",
	settings.EventUserPromptSubmit: "userPromptSubmitted",
	settings.EventStop:             "agentStop",
	settings.EventSessionEnd:       "sessionEnd",
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
	for _, s := range settings.SpecsFor("copilot", "native") {
		event := agentEvents[s.Event]
		if event == "" {
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

// mergeCopilotHooks is mergeCodexHooks for Copilot's flat (non-grouped) hook entries and its top-level
// {"version", "hooks"} shape: each entry is either kept whole, byte-identical (foreign) or dropped whole
// (harness-owned, by its "bash" command prefix); every other top-level key survives untouched.
func mergeCopilotHooks(existingRaw, body []byte) ([]byte, error) {
	next, renderedTop, err := render.MergeHooksTop(existingRaw, body, classifyCopilotEntry)
	if err != nil {
		return nil, err
	}
	var version int
	_ = json.Unmarshal(renderedTop["version"], &version) // absent/malformed rendered version: fall through to 1
	if version == 0 {
		version = 1
	}
	versionRaw, err := textutil.MarshalNoEscape(version)
	if err != nil {
		return nil, err
	}
	next["version"] = versionRaw
	return textutil.MarshalIndentNoEscape(next)
}

// classifyCopilotEntry reports an entry's foreign remainder (nil when harness-owned, by its "bash"
// command prefix via IsHarness) and whether it was harness-owned; a foreign entry is returned
// byte-identical so powershell/cwd/env and any other field survive untouched.
func classifyCopilotEntry(e json.RawMessage) (*json.RawMessage, bool) {
	var id struct {
		Bash string `json:"bash"`
	}
	if err := json.Unmarshal(e, &id); err == nil && settings.IsHarness(id.Bash) {
		return nil, true
	}
	return &e, false
}
