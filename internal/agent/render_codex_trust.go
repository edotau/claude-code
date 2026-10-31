package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/settings"
)

// Codex hashes sorted compact JSON of event_name and a group containing one normalized handler (verified
// on 0.163.0). codexTrustEntries reads mergedHooks — the real post-merge hooks.json, not the isolated
// rendered body — so gi/hi land on the position Codex itself will read; it emits an entry only for a
// harness-owned hook (IsHarness), leaving whatever trust Codex/Orca already recorded for a
// foreign hook (Orca, claude-dbx) alone. An event is checked against Codex's supported set only once it
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
	var canonical bytes.Buffer
	enc := json.NewEncoder(&canonical)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return "", err
	}
	digest := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte{'\n'}))
	return fmt.Sprintf("sha256:%x", digest), nil
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
	flush := func() {
		if hash, ok := remaining[active]; ok {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteByte('\n')
			}
			fmt.Fprintf(&out, "trusted_hash = %q\n", hash)
			delete(remaining, active)
		}
	}
	for _, line := range strings.SplitAfter(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		// A header only counts at depth 0: a "["-led line inside an open array (e.g. extra = [[1, 2]]) is not one.
		if state == "" && depth == 0 && strings.HasPrefix(trimmed, "[") {
			flush()
			active = codexTrustTableKey(trimmed)
		}
		if hash, ok := remaining[active]; ok && state == "" && depth == 0 {
			if replacement, replaced := replaceCodexTrustedHash(line, hash); replaced {
				line = replacement
				delete(remaining, active)
			}
		}
		out.WriteString(line)
		state, depth = tomlLineState(line, state, depth)
	}
	flush()
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	next := out.String()
	for _, key := range keys {
		section := fmt.Sprintf("[hooks.state.%q]\ntrusted_hash = %q\n", key, remaining[key])
		next = appendTOML(next, section)
	}
	return next
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
