package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// CodexJSON renders only explicitly allowed hooks in Codex's Claude-compatible schema.
func CodexJSON() ([]byte, error) {
	events := map[string][]settings.MatcherGroup{}
	for _, s := range settings.SpecsFor("codex", "native") {
		h := settings.Hook{Type: "command", Command: settings.AgentCommand("codex", s), Timeout: s.Seconds(), Async: s.Async, StatusMessage: s.StatusMessage}
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
	next, _, err := render.MergeHooksTop(existingRaw, body, classifyCodexGroup)
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
	obj, err := render.DecodeRawObject(raw)
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

func renderCodex(reg *providers.Registry, sel providers.Selection, o render.RenderOptions, out io.Writer) (bool, error) {
	body, err := CodexJSON()
	if err != nil {
		return false, err
	}
	path, raw, existed, err := readCodexConfig(o.Home)
	if err != nil {
		return false, err
	}
	next, err := codexProviderConfig(reg, sel, string(raw), out)
	if err != nil {
		return false, err
	}

	// Compute the hooks merge (and read the existing hooks.json) before computing trust entries or
	// committing config.toml: trust keys must come from the real post-merge positions, and a malformed
	// existing hooks.json must fail the whole render, not leave new trust hashes written against a
	// hooks.json that never got merged.
	hooksPath := filepath.Join(filepath.Dir(path), "hooks.json")
	hooksRaw, mergedHooks, hooksExisted, err := prepareCodexHooks(hooksPath, body)
	if err != nil {
		return false, err
	}

	next, err = codexTrustAndSettings(next, hooksPath, mergedHooks)
	if err != nil {
		return false, err
	}

	return commitCodexFiles(hooksPath, hooksRaw, mergedHooks, hooksExisted, path, raw, next, existed, o, out)
}

// commitCodexFiles writes hooks.json first, so a failed hooks write leaves config.toml without trust hashes for it.
func commitCodexFiles(hooksPath string, hooksRaw, mergedHooks []byte, hooksExisted bool,
	path string, raw []byte, next string, existed bool, o render.RenderOptions, out io.Writer) (bool, error) {
	drift, err := render.CommitFile(hooksPath, hooksRaw, append(mergedHooks, '\n'), hooksExisted, o, out, "codex-hooks")
	if err != nil {
		return drift, err
	}
	d, err := render.CommitFile(path, raw, []byte(next), existed, o, out, "codex")
	return drift || d, err
}

// readCodexConfig returns the config.toml path, its bytes and whether it existed.
func readCodexConfig(home string) (path string, raw []byte, existed bool, err error) {
	path, err = paths.CodexConfigPath(home)
	if err != nil {
		return "", nil, false, err
	}
	raw, err = os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, false, fmt.Errorf("codex: %w", err)
	}
	return path, raw, err == nil, nil
}

// codexProviderConfig is raw with the provider section inserted, any prior banner removed.
func codexProviderConfig(reg *providers.Registry, sel providers.Selection, raw string, out io.Writer) (string, error) {
	stripped := codexBannerRE().ReplaceAllString(raw, "") // sans any prior banner, for the user-value check
	section, err := codexProviderSection(reg, sel, stripped, out)
	if err != nil {
		return "", err
	}
	return insertCodexSection(raw, section), nil
}

func insertCodexSection(raw, section string) string {
	if section == "" {
		return raw
	}
	return agentconfig.InsertTopLevelTOML(codexBannerRE().ReplaceAllString(raw, ""), section)
}

func prepareCodexHooks(path string, body []byte) ([]byte, []byte, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, fmt.Errorf("codex-hooks: %w", err)
	}
	existed := err == nil
	merged, err := mergeCodexHooks(raw, body)
	if err != nil {
		return nil, nil, false, fmt.Errorf("codex-hooks: %w", err)
	}
	return raw, merged, existed, nil
}

func codexTrustAndSettings(next, hooksPath string, mergedHooks []byte) (string, error) {
	entries, err := CodexTrustEntries(hooksPath, mergedHooks)
	if err != nil {
		return "", err
	}
	next = MergeCodexTrust(next, entries)
	prefs, err := LoadCodexPreferences()
	if err != nil {
		return "", err
	}
	return patchCodexSettings(next, prefs)
}

// writeCodexHooks writes hooks.json under a file lock, refusing when it changed since the merge read it.
func writeCodexHooks(path string, old, next []byte, existed bool) error {
	return paths.WithFileLock(path, func() error {
		current, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if (err == nil) != existed || !bytes.Equal(current, old) {
			return errors.New("codex-hooks: hooks.json changed before write; retry to preserve the newer hooks")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if existed {
			if _, err := paths.Backup(path, old); err != nil {
				return err
			}
		}
		return paths.AtomicWrite(path, next, 0o600)
	})
}
