package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

var agentEvents = map[string]string{
	settings.EventPreToolUse:       "preToolUse",
	settings.EventPostToolUse:      "postToolUse",
	settings.EventUserPromptSubmit: "userPromptSubmitted",
}

// CodexJSON renders only explicitly allowed hooks in Codex's Claude-compatible schema.
func CodexJSON() ([]byte, error) {
	events := map[string][]settings.MatcherGroup{}
	for _, s := range settings.Specs() {
		if !slices.Contains(s.Vendors, "codex") {
			continue
		}
		h := settings.Hook{Type: "command", Command: settings.AgentEnv + "=codex " + s.Command(), Timeout: s.Seconds(), Async: s.Async, StatusMessage: s.StatusMessage}
		groups := events[s.Event]
		if n := len(groups); n > 0 && groups[n-1].Matcher == s.Matcher {
			groups[n-1].Hooks = append(groups[n-1].Hooks, h)
		} else {
			events[s.Event] = append(groups, settings.MatcherGroup{Matcher: s.Matcher, Hooks: []settings.Hook{h}})
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
	for _, s := range settings.Specs() {
		event := agentEvents[s.Event]
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
	return commitFile(path, raw, append(next, '\n'), existed, o, out, label)
}

// decodeRawObject decodes raw as a JSON object into map[string]json.RawMessage, preserving every key
// (known or not) as raw bytes; blank/absent raw decodes to an empty map rather than an error.
func decodeRawObject(raw []byte) (map[string]json.RawMessage, error) {
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

// decodeRawEvents decodes a "hooks" field (event name -> array of raw group/entry objects); a nil or
// absent field decodes to an empty map.
func decodeRawEvents(raw json.RawMessage) (map[string][]json.RawMessage, error) {
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

// mergeCodexHooks keeps every foreign top-level key, event and hook already in a Codex hooks.json,
// replacing only the harness's own groups (by command prefix, IsHarness) with the freshly
// rendered ones; mirrors internal/settings mergeHooks, adapted to the top-level {"hooks": {...}} file
// Codex reads directly.
func mergeCodexHooks(existingRaw, body []byte) ([]byte, error) {
	existingTop, err := decodeRawObject(existingRaw)
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err := decodeRawObject(body)
	if err != nil {
		return nil, err
	}
	existingHooks, err := decodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := decodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, err
	}
	merged := mergeHookEvents(existingHooks, renderedHooks, classifyCodexGroup)
	hooksRaw, err := settings.MarshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	return settings.MarshalIndentNoEscape(next)
}

// mergeHookEvents keeps every existing item of each event in its original relative position, dropping
// an item's harness-owned part per classify; the freshly rendered block is spliced in as a whole at the
// position of the FIRST existing item classify found harness-owned (prepended when none was); an event
// left with nothing is dropped. Idempotent: merging an already-merged event over the same rendered
// content reproduces it byte-for-byte, since the rendered block is itself wholly harness-owned next time
// round. Shared by mergeCodexHooks/mergeCopilotHooks.
func mergeHookEvents[T any](existing, rendered map[string][]T, classify func(T) (*T, bool)) map[string][]T {
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

// classifyCodexGroup reports a group's foreign remainder (nil if every hook in it was harness-owned, by
// "command" prefix via IsHarness) and whether it contained any harness-owned hook at all. A
// group with no harness hook is returned byte-identical (unknown group-level keys included); only a
// partially-harness group is rebuilt, which preserves every key but loses the original key order.
func classifyCodexGroup(raw json.RawMessage) (*json.RawMessage, bool) {
	var shape struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return &raw, false // not a hook-group object: leave it untouched rather than drop it silently
	}
	var foreign []json.RawMessage
	harnessFound := false
	for _, h := range shape.Hooks {
		var id struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(h, &id); err == nil && settings.IsHarness(id.Command) {
			harnessFound = true
			continue
		}
		foreign = append(foreign, h)
	}
	if !harnessFound {
		return &raw, false // no harness hook: keep the group byte-identical
	}
	if len(foreign) == 0 {
		return nil, true // every hook in the group was harness-owned: drop the whole group
	}
	rebuilt, err := rebuildCodexGroup(raw, foreign)
	if err != nil {
		return &raw, true
	}
	return &rebuilt, true
}

// rebuildCodexGroup replaces a partially-filtered group's "hooks" array, preserving every other key
// (matcher, or anything unknown) as raw bytes.
func rebuildCodexGroup(raw json.RawMessage, foreign []json.RawMessage) (json.RawMessage, error) {
	obj, err := decodeRawObject(raw)
	if err != nil {
		return nil, err
	}
	hooksRaw, err := settings.MarshalNoEscape(foreign)
	if err != nil {
		return nil, err
	}
	obj["hooks"] = hooksRaw
	return settings.MarshalNoEscape(obj)
}

// mergeCopilotHooks is mergeCodexHooks for Copilot's flat (non-grouped) hook entries and its top-level
// {"version", "hooks"} shape: each entry is either kept whole, byte-identical (foreign) or dropped whole
// (harness-owned, by its "bash" command prefix); every other top-level key survives untouched.
func mergeCopilotHooks(existingRaw, body []byte) ([]byte, error) {
	existingTop, err := decodeRawObject(existingRaw)
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err := decodeRawObject(body)
	if err != nil {
		return nil, err
	}
	existingHooks, err := decodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := decodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, err
	}
	merged := mergeHookEvents(existingHooks, renderedHooks, classifyCopilotEntry)
	hooksRaw, err := settings.MarshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	var version int
	_ = json.Unmarshal(renderedTop["version"], &version) // absent/malformed rendered version: fall through to 1
	if version == 0 {
		version = 1
	}
	versionRaw, err := settings.MarshalNoEscape(version)
	if err != nil {
		return nil, err
	}
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	next["version"] = versionRaw
	return settings.MarshalIndentNoEscape(next)
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

func renderCodex(reg *providers.Registry, sel providers.Selection, o RenderOptions, out io.Writer) (bool, error) {
	body, err := CodexJSON()
	if err != nil {
		return false, err
	}
	path, err := CodexConfigPath(o.Home)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("codex: %w", err)
	}
	existed := err == nil
	stripped := codexBannerRE().ReplaceAllString(string(raw), "") // sans any prior banner, for the user-value check
	section, err := codexProviderSection(reg, sel, stripped, out)
	if err != nil {
		return false, err
	}
	next := string(raw)
	if section != "" {
		next = codexBannerRE().ReplaceAllString(next, "") // strip any prior banner, then reinsert at the top-level spot
		next = insertTopLevelTOML(next, section)
	}

	// Compute the hooks merge (and read the existing hooks.json) before computing trust entries or
	// committing config.toml: trust keys must come from the real post-merge positions, and a malformed
	// existing hooks.json must fail the whole render, not leave new trust hashes written against a
	// hooks.json that never got merged.
	hooksPath := filepath.Join(filepath.Dir(path), "hooks.json")
	hooksRaw, err := os.ReadFile(hooksPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("codex-hooks: %w", err)
	}
	hooksExisted := err == nil
	mergedHooks, err := mergeCodexHooks(hooksRaw, body)
	if err != nil {
		return false, fmt.Errorf("codex-hooks: %w", err)
	}

	entries, err := codexTrustEntries(hooksPath, mergedHooks)
	if err != nil {
		return false, err
	}
	next = mergeCodexTrust(next, entries)
	overlay, err := LoadCodexOverlay()
	if err != nil {
		return false, err
	}
	next, err = patchCodexSettings(next, overlay)
	if err != nil {
		return false, err
	}

	drift, err := commitFile(path, raw, []byte(next), existed, o, out, "codex")
	if err != nil {
		return drift, err
	}
	d, err := commitFile(hooksPath, hooksRaw, append(mergedHooks, '\n'), hooksExisted, o, out, "codex-hooks")
	return drift || d, err
}
