package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// CodexJSON renders only explicitly allowed hooks in Codex's Claude-compatible schema.
func CodexJSON() ([]byte, error) {
	events := map[string][]settings.MatcherGroup{}
	for _, s := range settings.Specs() {
		if !slices.Contains(s.Agents, "codex") {
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

// mergeCodexHooks keeps every foreign top-level key, event and hook already in a Codex hooks.json,
// replacing only the harness's own groups (by command prefix, IsHarness) with the freshly
// rendered ones; mirrors internal/settings mergeHooks, adapted to the top-level {"hooks": {...}} file
// Codex reads directly.
func mergeCodexHooks(existingRaw, body []byte) ([]byte, error) {
	existingTop, err := agent.DecodeRawObject(existingRaw)
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedTop, err := agent.DecodeRawObject(body)
	if err != nil {
		return nil, err
	}
	existingHooks, err := agent.DecodeRawEvents(existingTop["hooks"])
	if err != nil {
		return nil, fmt.Errorf("existing file: %w", err)
	}
	renderedHooks, err := agent.DecodeRawEvents(renderedTop["hooks"])
	if err != nil {
		return nil, err
	}
	merged := agent.MergeHookEvents(existingHooks, renderedHooks, classifyCodexGroup)
	hooksRaw, err := textutil.MarshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	return textutil.MarshalIndentNoEscape(next)
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
	obj, err := agent.DecodeRawObject(raw)
	if err != nil {
		return nil, err
	}
	hooksRaw, err := textutil.MarshalNoEscape(foreign)
	if err != nil {
		return nil, err
	}
	obj["hooks"] = hooksRaw
	return textutil.MarshalNoEscape(obj)
}

func renderCodex(reg *providers.Registry, sel providers.Selection, o agent.RenderOptions, out io.Writer) (bool, error) {
	body, err := CodexJSON()
	if err != nil {
		return false, err
	}
	path, err := paths.CodexConfigPath(o.Home)
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
		next = agentconfig.InsertTopLevelTOML(next, section)
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

	entries, err := CodexTrustEntries(hooksPath, mergedHooks)
	if err != nil {
		return false, err
	}
	next = MergeCodexTrust(next, entries)
	overlay, err := LoadCodexOverlay()
	if err != nil {
		return false, err
	}
	next, err = patchCodexSettings(next, overlay)
	if err != nil {
		return false, err
	}

	drift, err := agent.CommitFile(path, raw, []byte(next), existed, o, out, "codex")
	if err != nil {
		return drift, err
	}
	d, err := agent.CommitFile(hooksPath, hooksRaw, append(mergedHooks, '\n'), hooksExisted, o, out, "codex-hooks")
	return drift || d, err
}
