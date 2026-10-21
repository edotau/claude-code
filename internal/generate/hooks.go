package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/providers"
)

// renderHooksFile writes body to path, merging it over whatever is already there when merge is
// non-nil: a nil merge overwrites outright (nothing currently needs that for a hooks file).
func renderHooksFile(path string, body []byte, merge func(existing, body []byte) ([]byte, error), o Options, out io.Writer, label string) (bool, error) {
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

// marshalNoEscape is json.Marshal with HTML-escaping disabled, so a foreign hook's literal "<"/">"/"&"
// survives unchanged instead of becoming "<" etc; compact, same as json.Marshal otherwise.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// marshalIndentNoEscape is json.MarshalIndent without HTML-escaping (json.Indent only reformats
// whitespace, it never re-escapes the bytes marshalNoEscape already produced).
func marshalIndentNoEscape(v any) ([]byte, error) {
	compact, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// mergeCodexHooks keeps every foreign top-level key, event and hook already in a Codex hooks.json,
// replacing only the harness's own groups (by command prefix, hookspec.IsHarness) with the freshly
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
	merged := mergeHookEvents(existingHooks, renderedHooks, foreignCodexGroups)
	hooksRaw, err := marshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	return marshalIndentNoEscape(next)
}

// mergeHookEvents keeps the foreign (non-harness) entries of each existing event, then prepends the
// freshly rendered ones; an event left with nothing is dropped. Shared by mergeCodexHooks/mergeCopilotHooks.
func mergeHookEvents[T any](existing, rendered map[string][]T, foreign func([]T) []T) map[string][]T {
	merged := map[string][]T{}
	for event, items := range existing {
		merged[event] = foreign(items)
	}
	for event, items := range rendered {
		merged[event] = append(append([]T{}, items...), merged[event]...)
	}
	for event, items := range merged {
		if len(items) == 0 {
			delete(merged, event)
		}
	}
	return merged
}

// foreignCodexGroups drops harness-owned hooks from each group (by "command" prefix, hookspec.IsHarness),
// keeping a group only while it still has at least one foreign hook left. A group the filter leaves
// untouched is kept as its original raw bytes, byte-identical (unknown group-level keys included); only a
// partially-filtered group is rebuilt, which preserves every key but loses the original key order.
func foreignCodexGroups(groups []json.RawMessage) []json.RawMessage {
	var kept []json.RawMessage
	for _, raw := range groups {
		var shape struct {
			Hooks []json.RawMessage `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &shape); err != nil {
			kept = append(kept, raw) // not a hook-group object: leave it untouched rather than drop it silently
			continue
		}
		var foreign []json.RawMessage
		for _, h := range shape.Hooks {
			var id struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(h, &id); err == nil && hookspec.IsHarness(id.Command) {
				continue
			}
			foreign = append(foreign, h)
		}
		switch {
		case len(shape.Hooks) > 0 && len(foreign) == 0:
			continue // every hook in the group was harness-owned: drop the whole group
		case len(foreign) == len(shape.Hooks):
			kept = append(kept, raw) // nothing filtered: keep the group byte-identical
		default:
			rebuilt, err := rebuildCodexGroup(raw, foreign)
			if err != nil {
				kept = append(kept, raw)
				continue
			}
			kept = append(kept, rebuilt)
		}
	}
	return kept
}

// rebuildCodexGroup replaces a partially-filtered group's "hooks" array, preserving every other key
// (matcher, or anything unknown) as raw bytes.
func rebuildCodexGroup(raw json.RawMessage, foreign []json.RawMessage) (json.RawMessage, error) {
	obj, err := decodeRawObject(raw)
	if err != nil {
		return nil, err
	}
	hooksRaw, err := marshalNoEscape(foreign)
	if err != nil {
		return nil, err
	}
	obj["hooks"] = hooksRaw
	return marshalNoEscape(obj)
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
	merged := mergeHookEvents(existingHooks, renderedHooks, foreignCopilotEntries)
	hooksRaw, err := marshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	var version int
	_ = json.Unmarshal(renderedTop["version"], &version) // absent/malformed rendered version: fall through to 1
	if version == 0 {
		version = 1
	}
	versionRaw, err := marshalNoEscape(version)
	if err != nil {
		return nil, err
	}
	next := maps.Clone(existingTop)
	if next == nil {
		next = map[string]json.RawMessage{}
	}
	next["hooks"] = hooksRaw
	next["version"] = versionRaw
	return marshalIndentNoEscape(next)
}

// foreignCopilotEntries drops harness-owned entries (by their "bash" command prefix), keeping every
// other entry as raw bytes so powershell/cwd/env and any other field survive untouched.
func foreignCopilotEntries(entries []json.RawMessage) []json.RawMessage {
	var kept []json.RawMessage
	for _, e := range entries {
		var id struct {
			Bash string `json:"bash"`
		}
		if err := json.Unmarshal(e, &id); err == nil && hookspec.IsHarness(id.Bash) {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

func renderCodex(reg *providers.Registry, sel providers.Selection, home string, o Options, out io.Writer) (bool, error) {
	body, err := hookspec.CodexJSON()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".codex", "config.toml")
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
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	entries, err := codexTrustEntries(hooksPath, body)
	if err != nil {
		return false, err
	}
	next = mergeCodexTrust(next, entries)

	// Compute the hooks merge (and read the existing hooks.json) before committing config.toml: a
	// malformed existing hooks.json must fail the whole render, not leave new trust hashes written
	// against a hooks.json that never got merged.
	hooksRaw, err := os.ReadFile(hooksPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("codex-hooks: %w", err)
	}
	hooksExisted := err == nil
	mergedHooks, err := mergeCodexHooks(hooksRaw, body)
	if err != nil {
		return false, fmt.Errorf("codex-hooks: %w", err)
	}

	drift, err := commitFile(path, raw, []byte(next), existed, o, out, "codex")
	if err != nil {
		return drift, err
	}
	d, err := commitFile(hooksPath, hooksRaw, append(mergedHooks, '\n'), hooksExisted, o, out, "codex-hooks")
	return drift || d, err
}

// Codex hashes sorted compact JSON of event_name and a group containing one normalized handler (verified on 0.163.0).
func codexTrustEntries(path string, body []byte) (map[string]string, error) {
	var config struct {
		Hooks map[string][]hookspec.Group `json:"hooks"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, err
	}
	events := map[string]string{
		hookspec.EventPreToolUse: "pre_tool_use", hookspec.EventPostToolUse: "post_tool_use",
		hookspec.EventUserPromptSubmit: "user_prompt_submit",
	}
	path = codexHookSourcePath(path)
	entries := map[string]string{}
	for event, groups := range config.Hooks {
		name, ok := events[event]
		if !ok {
			return nil, fmt.Errorf("codex: unsupported hook event %q", event)
		}
		for gi, group := range groups {
			for hi, hook := range group.Hooks {
				handler := map[string]any{
					"type": hook.Type, "command": hook.Command, "timeout": max(1, hook.Timeout), "async": hook.Async,
				}
				if hook.StatusMessage != "" {
					handler["statusMessage"] = hook.StatusMessage
				}
				identity := map[string]any{"event_name": name, "hooks": []any{handler}}
				if event != hookspec.EventUserPromptSubmit && group.Matcher != "" {
					identity["matcher"] = group.Matcher
				}
				var canonical bytes.Buffer
				enc := json.NewEncoder(&canonical)
				enc.SetEscapeHTML(false)
				if err := enc.Encode(identity); err != nil {
					return nil, err
				}
				digest := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte{'\n'}))
				key := fmt.Sprintf("%s:%s:%d:%d", path, name, gi, hi)
				entries[key] = fmt.Sprintf("sha256:%x", digest)
			}
		}
	}
	return entries, nil
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

func appendTOML(raw, section string) string {
	if raw == "" {
		return section
	}
	if !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	return raw + "\n" + section
}

// insertTopLevelTOML puts section before the first real top-level table header (outside multiline
// strings and open arrays), dropping any pre-existing top-level assignment section itself also sets,
// so TOML never ends up with duplicate keys; with no such header in raw, it falls back to appendTOML.
func insertTopLevelTOML(raw, section string) string {
	keys := topLevelKeysOf(section)
	lines := strings.SplitAfter(raw, "\n")
	idx := firstTopLevelHeaderIndex(lines)
	if idx == -1 {
		return appendTOML(dropTopLevelKeys(raw, keys), section)
	}
	before := dropTopLevelKeys(strings.Join(lines[:idx], ""), keys)
	before = strings.TrimRight(before, "\n")
	after := strings.Join(lines[idx:], "")
	if before != "" {
		before += "\n\n"
	}
	return before + section + "\n" + after
}

// topLevelKeysOf returns the bare key names section assigns before its own first table header, i.e.
// the keys an insert must not duplicate in whatever top-level text already precedes it.
func topLevelKeysOf(section string) []string {
	lines := strings.SplitAfter(section, "\n")
	idx := firstTopLevelHeaderIndex(lines)
	if idx == -1 {
		idx = len(lines)
	}
	var keys []string
	for _, line := range lines[:idx] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, ok := strings.Cut(trimmed, "="); ok {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	return keys
}

// dropTopLevelKeys removes whole lines (including multiline/array continuations) that assign one of
// keys at true top level (depth 0, outside multiline strings), leaving same-named keys inside a table.
func dropTopLevelKeys(raw string, keys []string) string {
	if len(keys) == 0 || raw == "" {
		return raw
	}
	lines := strings.SplitAfter(raw, "\n")
	var out strings.Builder
	multiline, depth := "", 0
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		drop := skipping
		if !skipping && multiline == "" && depth == 0 {
			if key, _, ok := strings.Cut(trimmed, "="); ok && slices.Contains(keys, strings.TrimSpace(key)) {
				drop = true
			}
		}
		nextMultiline, nextDepth := tomlLineState(line, multiline, depth)
		if drop {
			skipping = nextMultiline != "" || nextDepth > 0
		} else {
			out.WriteString(line)
		}
		multiline, depth = nextMultiline, nextDepth
	}
	return out.String()
}

// firstTopLevelHeaderIndex finds the first line whose "[" is a genuine top-level table header: outside
// any multiline string and outside an open (possibly multiline) array; -1 when raw has none.
func firstTopLevelHeaderIndex(lines []string) int {
	multiline, depth := "", 0
	for i, line := range lines {
		if multiline == "" && depth == 0 && strings.HasPrefix(strings.TrimSpace(line), "[") {
			return i
		}
		multiline, depth = tomlLineState(line, multiline, depth)
	}
	return -1
}

// tomlLineState extends tomlMultilineState with bracket depth, so a "["-led continuation line inside
// an open (possibly multiline) array is never mistaken for a table header.
func tomlLineState(line, state string, depth int) (string, int) {
	for i := 0; i < len(line); {
		if state != "" {
			if state == `"""` && line[i] == '\\' {
				i += 2
			} else if strings.HasPrefix(line[i:], state) {
				i += 3
				state = ""
			} else {
				i++
			}
			continue
		}
		switch {
		case line[i] == '#':
			return state, depth
		case line[i] == '[':
			depth++
			i++
		case line[i] == ']':
			if depth > 0 {
				depth--
			}
			i++
		case line[i] != '"' && line[i] != '\'':
			i++
		default:
			quote := line[i]
			triple := strings.Repeat(string(quote), 3)
			if strings.HasPrefix(line[i:], triple) {
				state = triple
				i += 3
				continue
			}
			i++
			for i < len(line) {
				if line[i] == '\\' && quote == '"' {
					i += 2
				} else if line[i] == quote {
					i++
					break
				} else {
					i++
				}
			}
		}
	}
	return state, depth
}
