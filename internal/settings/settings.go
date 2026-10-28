// Package settings renders the harness settings.json and merges it into an existing one without
// dropping foreign keys (bar the vault drop list) or foreign hooks.
package settings

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
)

//go:embed template/*.json
var templateFS embed.FS

const templateDir = "template"

// Section groups the template keys owned by one section file (Name = file stem without its numeric prefix).
type Section struct {
	Name string
	Keys []string
}

// Sections returns the template sections in file-name (merge) order.
func Sections() ([]Section, error) {
	names, err := templateFileNames()
	if err != nil {
		return nil, err
	}
	sections := make([]Section, 0, len(names))
	for _, name := range names {
		raw, err := templateFS.ReadFile(path.Join(templateDir, name))
		if err != nil {
			return nil, err
		}
		o, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("settings template %s: %w", name, err)
		}
		keys := slices.DeleteFunc(slices.Clone(o.keys), func(k string) bool { return k == "$schema" })
		sections = append(sections, Section{Name: sectionName(name), Keys: keys})
	}
	return sections, nil
}

// mergeSections merges template/*.json from fsys in file-name order, expanding ${HOOK_BIN}. A top-level
// key defined in two section files is an error naming both files.
func mergeSections(fsys fs.FS) (*object, error) {
	entries, err := fs.ReadDir(fsys, templateDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	o := &object{vals: map[string]json.RawMessage{}}
	owner := map[string]string{}
	for _, name := range names {
		raw, err := fs.ReadFile(fsys, path.Join(templateDir, name))
		if err != nil {
			return nil, err
		}
		expanded := strings.ReplaceAll(string(raw), "${HOOK_BIN}", hookspec.HookBin)
		part, err := parseObject([]byte(expanded))
		if err != nil {
			return nil, fmt.Errorf("settings template %s: %w", name, err)
		}
		for _, k := range part.keys {
			if prev, dup := owner[k]; dup {
				return nil, fmt.Errorf("settings template: %q defined in both %s and %s", k, prev, name)
			}
			owner[k] = name
			o.set(k, part.vals[k])
		}
	}
	return o, nil
}

// sectionName strips the numeric prefix and extension: "10-ui.json" -> "ui".
func sectionName(fileName string) string {
	stem := strings.TrimSuffix(fileName, ".json")
	if _, after, ok := strings.Cut(stem, "-"); ok {
		return after
	}
	return stem
}

func templateFileNames() ([]string, error) {
	entries, err := fs.ReadDir(templateFS, templateDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Render merges the template sections in file-name order, expands ${HOOK_BIN}, and adds the hookspec hooks
// block. A top-level key defined in two section files is an error naming both files.
func Render() ([]byte, error) {
	o, err := mergeSections(templateFS)
	if err != nil {
		return nil, err
	}
	hooks, err := hookspec.SettingsJSON()
	if err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	return o.marshal()
}

// Summary describes what Merge did, for `install --dry-run`.
type Summary struct {
	Kept         []string // existing top-level keys the template does not own
	Retired      []string // existing keys the harness removes (retiredKeys)
	Dropped      []string // existing keys removed by the vault drop list
	Owned        []string // template keys written (env/permissions unioned, others replaced)
	ForeignHooks int      // non-harness hook commands preserved
	HarnessHooks int
}

// retiredKeys are dropped from existing settings: autoMemoryDirectory is flat, so it merges every repo's memory.
var retiredKeys = []string{"autoMemoryDirectory"}

// runtimeKeys are written by Claude Code itself (e.g. /effort → modelSettings): merge keeps them, drift ignores them.
var runtimeKeys = []string{"modelSettings"}

// retiredEnvKeys are env keys a template once set; merge would keep them after the template drops them.
var retiredEnvKeys = []string{"CLAUDE_CODE_DISABLE_VIRTUAL_SCROLL"}

// Merge overlays rendered harness settings onto existing: foreign keys kept in place, env and permission
// lists unioned, harness hooks replaced, foreign hooks kept after them; keys in dropped are removed.
func Merge(existing, rendered []byte, dropped []string) ([]byte, Summary, error) {
	var sum Summary
	if len(bytes.TrimSpace(existing)) == 0 {
		existing = []byte("{}")
	}
	ex, err := parseObject(existing)
	if err != nil {
		return nil, sum, fmt.Errorf("existing settings: %w", err)
	}
	tp, err := parseObject(rendered)
	if err != nil {
		return nil, sum, err
	}
	kept := &object{vals: map[string]json.RawMessage{}}
	for _, k := range ex.keys {
		_, owned := tp.vals[k]
		switch {
		case slices.Contains(retiredKeys, k):
			sum.Retired = append(sum.Retired, k)
			continue
		case !owned && slices.Contains(dropped, k): // a template key is never dropped: its existing value still merges
			sum.Dropped = append(sum.Dropped, k)
			continue
		case !owned && !slices.Contains(runtimeKeys, k):
			sum.Kept = append(sum.Kept, k)
		}
		kept.set(k, ex.vals[k])
	}
	ex = kept
	for _, k := range tp.keys {
		sum.Owned = append(sum.Owned, k)
		v := tp.vals[k]
		var merged json.RawMessage
		switch k {
		case "env":
			if merged, err = mergeObject(ex.vals[k], v); err == nil {
				merged, err = dropRetiredEnv(merged, &sum)
			}
		case "permissions":
			merged, err = mergePermissions(ex.vals[k], v)
		case "hooks":
			merged, err = mergeHooks(ex.vals[k], v, &sum)
		default:
			merged = v
		}
		if err != nil {
			return nil, sum, fmt.Errorf("merge %s: %w", k, err)
		}
		ex.set(k, merged)
	}
	out, err := ex.marshal()
	return out, sum, err
}

// dropRetiredEnv removes keys listed in retiredEnvKeys, recording each as retired.
func dropRetiredEnv(raw json.RawMessage, sum *Summary) (json.RawMessage, error) {
	o, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	kept := &object{vals: map[string]json.RawMessage{}}
	for _, k := range o.keys {
		if slices.Contains(retiredEnvKeys, k) {
			sum.Retired = append(sum.Retired, "env."+k)
			continue
		}
		kept.set(k, o.vals[k])
	}
	return kept.marshal()
}

// mergeObject sets every overlay key onto base, keeping base's other keys and order.
func mergeObject(base, overlay json.RawMessage) (json.RawMessage, error) {
	b, err := parseObject(orEmpty(base))
	if err != nil {
		return nil, err
	}
	o, err := parseObject(overlay)
	if err != nil {
		return nil, err
	}
	for _, k := range o.keys {
		b.set(k, o.vals[k])
	}
	return b.marshal()
}

// mergePermissions unions the allow/ask/deny lists; other existing permission keys stay.
func mergePermissions(base, overlay json.RawMessage) (json.RawMessage, error) {
	b, err := parseObject(orEmpty(base))
	if err != nil {
		return nil, err
	}
	o, err := parseObject(overlay)
	if err != nil {
		return nil, err
	}
	for _, k := range o.keys {
		var have, add []string
		if json.Unmarshal(o.vals[k], &add) != nil {
			b.set(k, o.vals[k])
			continue
		}
		if raw, ok := b.vals[k]; ok && json.Unmarshal(raw, &have) != nil {
			return nil, fmt.Errorf("permissions.%s is not a string list", k)
		}
		for _, r := range add {
			if !slices.Contains(have, r) {
				have = append(have, r)
			}
		}
		raw, err := encode(have)
		if err != nil {
			return nil, err
		}
		b.set(k, raw)
	}
	return b.marshal()
}

// mergeHooks strips harness commands from existing events, then writes harness groups followed by the
// foreign ones; foreign groups and hooks keep their bytes and field order.
func mergeHooks(base, overlay json.RawMessage, sum *Summary) (json.RawMessage, error) {
	b, err := parseObject(orEmpty(base))
	if err != nil {
		return nil, err
	}
	o, err := parseObject(overlay)
	if err != nil {
		return nil, err
	}
	events := map[string][]json.RawMessage{}
	for _, ev := range b.keys {
		events[ev], err = foreignHookGroups(ev, b.vals[ev], sum)
		if err != nil {
			return nil, err
		}
	}
	for _, ev := range o.keys {
		var groups []hookspec.Group
		if err := json.Unmarshal(o.vals[ev], &groups); err != nil {
			return nil, err
		}
		var raws []json.RawMessage
		for _, g := range groups {
			sum.HarnessHooks += len(g.Hooks)
			raw, err := encode(g)
			if err != nil {
				return nil, err
			}
			raws = append(raws, raw)
		}
		events[ev] = append(raws, events[ev]...)
		if _, ok := b.vals[ev]; !ok {
			b.keys = append(b.keys, ev)
		}
	}
	out := &object{vals: map[string]json.RawMessage{}}
	for _, ev := range b.keys {
		if len(events[ev]) > 0 {
			out.set(ev, rawList(events[ev]))
		}
	}
	return out.marshal()
}

func foreignHookGroups(event string, raw json.RawMessage, sum *Summary) ([]json.RawMessage, error) {
	var groups, foreign []json.RawMessage
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("hooks.%s: %w", event, err)
	}
	for _, raw := range groups {
		g, err := parseObject(raw)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", event, err)
		}
		var hooks, keep []json.RawMessage
		if raw := g.vals["hooks"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &hooks); err != nil {
				return nil, fmt.Errorf("hooks.%s group: %w", event, err)
			}
		}
		for _, h := range hooks {
			var cmd struct {
				Command string `json:"command"`
			}
			_ = json.Unmarshal(h, &cmd)
			if !hookspec.IsHarness(cmd.Command) {
				keep = append(keep, h)
			}
		}
		if len(keep) == 0 {
			continue
		}
		sum.ForeignHooks += len(keep)
		g.set("hooks", rawList(keep))
		if raw, err = g.marshal(); err != nil {
			return nil, err
		}
		foreign = append(foreign, raw)
	}
	return foreign, nil
}

func rawList(items []json.RawMessage) json.RawMessage {
	parts := make([][]byte, len(items))
	for i, it := range items {
		parts[i] = bytes.TrimSpace(it)
	}
	return json.RawMessage("[" + string(bytes.Join(parts, []byte(","))) + "]")
}

// WriteWithBackup copies to the cache-mirrored backup path (numbered on collision), then writes data atomically.
func WriteWithBackup(path string, data []byte) (backup string, err error) {
	mode := os.FileMode(0o644)
	old, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
		base := paths.BackupPath(path, time.Now())
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			return "", fmt.Errorf("backup dir %s: %w", filepath.Dir(base), err)
		}
		backup, err = paths.WriteBackup(base, old)
		if err != nil {
			return "", fmt.Errorf("backup %s: %w", base, err)
		}
	case !os.IsNotExist(rerr):
		return "", fmt.Errorf("read %s: %w", path, rerr)
	}
	return backup, paths.AtomicWrite(path, data, mode)
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}
