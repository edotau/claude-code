package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// Codex clamps a SessionEnd hook to 3s; a longer one warns on every launch and never runs (observed on 0.166.0).
const codexSessionEndMax = 3

// CodexJSON renders only explicitly allowed hooks in Codex's Claude-compatible schema.
func CodexJSON() ([]byte, error) {
	events := map[string][]settings.MatcherGroup{}
	for _, s := range settings.SpecsFor("codex", "native") {
		timeout := s.Seconds()
		if s.Event == settings.EventSessionEnd {
			timeout = min(timeout, codexSessionEndMax)
		}
		h := settings.Hook{Type: "command", Command: settings.AgentCommand("codex", s), Timeout: timeout, StatusMessage: s.StatusMessage}
		groups, matcher := events[s.Event], codexMatcher(s.Matcher)
		if n := len(groups); n > 0 && groups[n-1].Matcher == matcher {
			groups[n-1].Hooks = append(groups[n-1].Hooks, h)
		} else {
			events[s.Event] = append(groups, settings.MatcherGroup{Matcher: matcher, Hooks: []settings.Hook{h}})
		}
	}
	return json.MarshalIndent(map[string]any{"hooks": events}, "", "  ")
}

// codexMatcher adds apply_patch, Codex's file-write tool, to a matcher selecting Claude's Write or Edit.
func codexMatcher(m string) string {
	for alt := range strings.SplitSeq(m, "|") {
		if alt == "Write" || alt == "Edit" {
			return m + "|apply_patch"
		}
	}
	return m
}

// mergeCodexHooks keeps every foreign top-level key, event and hook already in a Codex hooks.json,
// replacing only the harness's own groups (by command prefix, IsHarness) with the freshly
// rendered ones; mirrors internal/settings mergeHooks, adapted to the top-level {"hooks": {...}} file
// Codex reads directly.
func mergeCodexHooks(existingRaw, body []byte) ([]byte, error) {
	next, _, err := agentconfig.MergeHooksTop(existingRaw, body, classifyCodexGroup)
	if err != nil {
		return nil, err
	}
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
	obj, err := agentconfig.DecodeRawObject(raw)
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

func writeCodexHooks(path string, old, next []byte, existed bool) error {
	return withCodexLock(path, func() error {
		target, err := codexWriteTarget(path)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if (err == nil) != existed || !bytes.Equal(current, old) {
			return errors.New("codex-hooks: hooks.json changed before write; retry to preserve the newer hooks")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if existed {
			if _, err := paths.Backup(path, old); err != nil {
				return err
			}
		}
		return paths.AtomicWrite(target, next, 0o600)
	})
}

// Codex hashes sorted compact JSON of event_name and a group containing one normalized handler (verified
// on 0.163.0). codexTrustEntries reads mergedHooks — the real post-merge hooks.json, not the isolated
// rendered body — so gi/hi land on the position Codex itself will read; it emits an entry only for a
// harness-owned hook (IsHarness), leaving whatever trust Codex already recorded for a
// foreign hook (claude-dbx, ...) alone. An event is checked against Codex's supported set only once it
// is found to contain a harness hook, so a foreign-only event (Stop, SessionStart, ...) never errors.
func codexTrustEntries(path string, mergedHooks []byte) (map[string]string, error) {
	var config struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(mergedHooks, &config); err != nil {
		return nil, err
	}
	events := map[string]string{
		settings.EventPreToolUse: "pre_tool_use", settings.EventPostToolUse: "post_tool_use",
		settings.EventUserPromptSubmit: "user_prompt_submit",
		settings.EventSessionStart:     "session_start", settings.EventSubagentStart: "subagent_start",
		settings.EventStop: "stop", settings.EventSessionEnd: "session_end",
	}
	path = codexHookSourcePath(path)
	entries := map[string]string{}
	for event, groups := range config.Hooks {
		if !anyHarnessHook(groups) {
			continue
		}
		name, ok := events[event]
		if !ok {
			return nil, fmt.Errorf("codex: unsupported hook event %q", event)
		}
		if err := addCodexTrustEntries(entries, path, event, name, groups); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// anyHarnessHook reports whether groups contains at least one harness-owned hook.
func anyHarnessHook(groups []settings.MatcherGroup) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if settings.IsHarness(hook.Command) {
				return true
			}
		}
	}
	return false
}

// addCodexTrustEntries fills entries with one digest per harness-owned hook in groups.
func addCodexTrustEntries(entries map[string]string, path, event, name string, groups []settings.MatcherGroup) error {
	for gi, group := range groups {
		for hi, hook := range group.Hooks {
			if !settings.IsHarness(hook.Command) {
				continue
			}
			digest, err := codexTrustDigest(event, name, group, hook)
			if err != nil {
				return err
			}
			entries[fmt.Sprintf("%s:%s:%d:%d", path, name, gi, hi)] = digest
		}
	}
	return nil
}

// codexTrustDigest computes Codex's trust digest for one harness hook identity.
func codexTrustDigest(event, name string, group settings.MatcherGroup, hook settings.Hook) (string, error) {
	handler := map[string]any{
		"type": hook.Type, "command": hook.Command, "timeout": max(1, hook.Timeout), "async": hook.Async,
	}
	if hook.StatusMessage != "" {
		handler["statusMessage"] = hook.StatusMessage
	}
	identity := map[string]any{"event_name": name, "hooks": []any{handler}}
	if event != settings.EventUserPromptSubmit && group.Matcher != "" {
		identity["matcher"] = group.Matcher
	}
	canonical, err := textutil.MarshalNoEscape(identity)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(canonical)), nil
}

// Codex resolves the source directory through symlinks but retains the hooks.json leaf.
func codexHookSourcePath(path string) string {
	path, _ = filepath.Abs(path)
	parent := filepath.Dir(path)
	tail := filepath.Base(path)
	for {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, tail)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return path
		}
		tail = filepath.Join(filepath.Base(parent), tail)
		parent = next
	}
}

// Compiled lazily (sync.OnceValue): a package-level MustCompile runs at every process start (hooks, statusline).
var codexTrustTableRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^\[\s*(?:hooks|"hooks"|'hooks')\s*\.\s*(?:state|"state"|'state')\s*\.\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*\]\s*(?:#.*)?$`)
})
var codexTrustedHashRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(\s*(?:trusted_hash|"trusted_hash"|'trusted_hash')\s*=\s*)("(?:[^"\\]|\\.)*"|'[^']*')(.*)$`)
})

// mergeCodexTrust changes only the emitted hooks' hashes, retaining unrelated tables, fields, and comments.
func mergeCodexTrust(raw string, entries map[string]string) string {
	remaining := make(map[string]string, len(entries))
	maps.Copy(remaining, entries)
	var out strings.Builder
	active, state, depth := "", "", 0
	for _, line := range strings.SplitAfter(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		// A header only counts at depth 0: a "["-led line inside an open array (e.g. extra = [[1, 2]]) is not one.
		if state == "" && depth == 0 && strings.HasPrefix(trimmed, "[") {
			flushCodexTrust(&out, remaining, active)
			active = codexTrustTableKey(trimmed)
		}
		if state == "" && depth == 0 {
			line = mergeCodexTrustLine(line, remaining, active)
		}
		out.WriteString(line)
		state, depth = agentconfig.TOMLLineState(line, state, depth)
	}
	flushCodexTrust(&out, remaining, active)
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	next := out.String()
	for _, key := range keys {
		section := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q\n", key, remaining[key])
		next = agentconfig.AppendTOML(next, section)
	}
	return next
}

func mergeCodexTrustLine(line string, remaining map[string]string, active string) string {
	hash, ok := remaining[active]
	if !ok {
		return line
	}
	if replacement, replaced := replaceCodexTrustedHash(line, hash); replaced {
		delete(remaining, active)
		return replacement
	}
	return line
}

func flushCodexTrust(out *strings.Builder, remaining map[string]string, active string) {
	if hash, ok := remaining[active]; ok {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "trusted_hash = %q\n", hash)
		delete(remaining, active)
	}
}

func codexTrustTableKey(line string) string {
	m := codexTrustTableRE().FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	if strings.HasPrefix(m[1], "'") {
		return strings.Trim(m[1], "'")
	}
	key, _ := strconv.Unquote(m[1])
	return key
}

func replaceCodexTrustedHash(line, hash string) (string, bool) {
	m := codexTrustedHashRE().FindStringSubmatch(strings.TrimSuffix(line, "\n"))
	if m == nil {
		return line, false
	}
	ending := ""
	if strings.HasSuffix(line, "\n") {
		ending = "\n"
	}
	return m[1] + strconv.Quote(hash) + m[3] + ending, true
}
