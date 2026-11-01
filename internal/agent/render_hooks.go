package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/textutil"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/settings"
)

var agentEvents = map[string]string{
	settings.EventPreToolUse:       "preToolUse",
	settings.EventPostToolUse:      "postToolUse",
	settings.EventUserPromptSubmit: "userPromptSubmitted",
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
	for _, s := range settings.Specs() {
		event := agentEvents[s.Event]
		if !slices.Contains(s.Agents, "copilot") || event == "" {
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

// renderHooksFile writes body to path, merging it over whatever is already there when merge is
// non-nil: a nil merge overwrites outright (nothing currently needs that for a hooks file).
func renderHooksFile(path string, body []byte, merge func(existing, body []byte) ([]byte, error), o RenderOptions, out io.Writer, label string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%s: %w", label, err)
	}
	existed := err == nil
	next := body
	if merge != nil {
		if next, err = merge(raw, body); err != nil {
			return false, fmt.Errorf("%s: %w", label, err)
		}
	}
	return CommitFile(path, raw, append(next, '\n'), existed, o, out, label)
}

// DecodeRawObject decodes raw as a JSON object into map[string]json.RawMessage, preserving every key
// (known or not) as raw bytes; blank/absent raw decodes to an empty map rather than an error.
func DecodeRawObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// DecodeRawEvents decodes a "hooks" field (event name -> array of raw group/entry objects); a nil or
// absent field decodes to an empty map.
func DecodeRawEvents(raw json.RawMessage) (map[string][]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string][]json.RawMessage{}, nil
	}
	var m map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string][]json.RawMessage{}
	}
	return m, nil
}

// MergeHookEvents keeps every existing item of each event in its original relative position, dropping
// an item's harness-owned part per classify; the freshly rendered block is spliced in as a whole at the
// position of the FIRST existing item classify found harness-owned (prepended when none was); an event
// left with nothing is dropped. Idempotent: merging an already-merged event over the same rendered
// content reproduces it byte-for-byte, since the rendered block is itself wholly harness-owned next time
// round. Shared by mergeCodexHooks/mergeCopilotHooks.
func MergeHookEvents[T any](existing, rendered map[string][]T, classify func(T) (*T, bool)) map[string][]T {
	merged := map[string][]T{}
	for event, items := range existing {
		var out []T
		insertAt := -1
		for _, item := range items {
			remainder, harnessOwned := classify(item)
			if harnessOwned && insertAt == -1 {
				insertAt = len(out)
			}
			if remainder != nil {
				out = append(out, *remainder)
			}
		}
		if insertAt == -1 {
			insertAt = 0 // no harness-owned item in this event: prepend, as if nothing existed before
		}
		block := rendered[event]
		result := make([]T, 0, len(out)+len(block))
		result = append(result, out[:insertAt]...)
		result = append(result, block...)
		result = append(result, out[insertAt:]...)
		merged[event] = result
	}
	for event, items := range rendered {
		if _, ok := existing[event]; !ok {
			merged[event] = items
		}
	}
	for event, items := range merged {
		if len(items) == 0 {
			delete(merged, event)
		}
	}
	return merged
}

// mergeCopilotHooks is mergeCodexHooks for Copilot's flat (non-grouped) hook entries and its top-level
// {"version", "hooks"} shape: each entry is either kept whole, byte-identical (foreign) or dropped whole
// (harness-owned, by its "bash" command prefix); every other top-level key survives untouched.
func mergeCopilotHooks(existingRaw, body []byte) ([]byte, error) {
	existingTop, err := DecodeRawObject(existingRaw)
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err := DecodeRawObject(body)
	if err != nil {
		return nil, err
	}
	existingHooks, err := DecodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := DecodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, err
	}
	merged := MergeHookEvents(existingHooks, renderedHooks, classifyCopilotEntry)
	hooksRaw, err := textutil.MarshalNoEscape(merged)
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
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
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
