// Package settings renders the harness settings.json and refreshes the hooks of an existing one,
// leaving every other key and every foreign hook as the user has it.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/defaults"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// Section groups the default keys shown under one configure-page header.
type Section struct {
	Name string
	Keys []string
}

// sectionOrder is the header order; structuralSection homes the schema keys the catalog does not section.
var (
	sectionOrder      = []string{"model", "ui", "notifications", "session", "env", "more"}
	structuralSection = map[string]string{"model": "model"}
)

// Sections groups the editable keys by section: the defaults file's scalar keys and env variables first,
// then every sectioned catalog key the file lacks, so a key removed from the file stays on the page.
func Sections() ([]Section, error) {
	o, err := parseObject(defaults.Claude)
	if err != nil {
		return nil, fmt.Errorf("defaults/claude/settings.json: %w", err)
	}
	state := sectionState{by: map[string][]string{}, seen: map[string]bool{}}
	for _, k := range o.keys {
		if err := state.addDefault(k, o.vals[k]); err != nil {
			return nil, err
		}
	}
	for _, f := range Catalog() {
		if f.Section != "" {
			state.add(f.Section, f.Key)
		}
	}
	sections := make([]Section, 0, len(sectionOrder))
	for _, name := range sectionOrder {
		if keys := state.by[name]; len(keys) > 0 {
			sections = append(sections, Section{Name: name, Keys: keys})
		}
	}
	return sections, nil
}

type sectionState struct {
	by   map[string][]string
	seen map[string]bool
}

func (s *sectionState) add(name, key string) {
	if !s.seen[key] {
		s.seen[key] = true
		s.by[name] = append(s.by[name], key)
	}
}

func (s *sectionState) addDefault(k string, raw json.RawMessage) error {
	if k == "env" {
		return s.addEnv(raw)
	}
	f, ok := FieldFor(k)
	if !ok || f.Kind == KindObject || f.Kind == KindArray || CredentialKey(k) {
		return nil
	}
	name := f.Section
	if name == "" {
		name = structuralSection[k]
	}
	if name == "" {
		name = "more"
	}
	s.add(name, k)
	return nil
}

func (s *sectionState) addEnv(raw json.RawMessage) error {
	env, err := parseObject(raw)
	if err != nil {
		return fmt.Errorf("defaults/claude/settings.json env: %w", err)
	}
	for _, e := range env.keys {
		if _, ok := FieldFor(e); ok && !CredentialKey(e) {
			s.add("env", e)
		}
	}
	return nil
}

// loadDefaults parses the embedded defaults file, expanding ${HOOK_BIN}.
func loadDefaults() (*object, error) {
	expanded := strings.ReplaceAll(string(defaults.Claude), "${HOOK_BIN}", HookBin)
	o, err := parseObject([]byte(expanded))
	if err != nil {
		return nil, fmt.Errorf("defaults/claude/settings.json: %w", err)
	}
	return o, nil
}

// Render loads the defaults file, expands ${HOOK_BIN}, and adds the hook registry block.
func Render() ([]byte, error) {
	o, err := loadDefaults()
	if err != nil {
		return nil, err
	}
	hooks, err := SettingsJSON()
	if err != nil {
		return nil, err
	}
	o.set("hooks", hooks)
	return o.marshal()
}

// Summary describes what Merge did, for `install --dry-run`.
type Summary struct {
	Kept         []string // existing top-level keys the template does not own
	Drift        []string // keys other than hooks that differ from the defaults render (set by install)
	ForeignHooks int      // non-harness hook commands preserved
	HarnessHooks int
}

// runtimeKeys are written by Claude Code itself (e.g. /effort → modelSettings): merge keeps them, drift ignores them.
var runtimeKeys = []string{"modelSettings"}

// Merge refreshes only the hooks of an existing file (harness hooks replaced, foreign kept after them) and
// leaves every other key as is; an empty existing is a fresh file and takes every rendered key.
func Merge(existing, rendered []byte) ([]byte, Summary, error) {
	var sum Summary
	fresh := len(bytes.TrimSpace(existing)) == 0
	if fresh {
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
	sum.Kept = keptKeys(ex, tp)
	for _, k := range tp.keys {
		if k != "hooks" && !fresh {
			continue
		}
		v := tp.vals[k]
		if k == "hooks" {
			if v, err = mergeHooks(ex.vals[k], v, &sum); err != nil {
				return nil, sum, fmt.Errorf("merge %s: %w", k, err)
			}
		}
		ex.set(k, v)
	}
	out, err := ex.marshal()
	return out, sum, err
}

func keptKeys(ex, tp *object) []string {
	var kept []string
	for _, k := range ex.keys {
		if _, owned := tp.vals[k]; !owned && !slices.Contains(runtimeKeys, k) {
			kept = append(kept, k)
		}
	}
	return kept
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
		raws, err := harnessHookGroups(o.vals[ev], sum)
		if err != nil {
			return nil, err
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

func harnessHookGroups(raw json.RawMessage, sum *Summary) ([]json.RawMessage, error) {
	var groups []MatcherGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	var raws []json.RawMessage
	for _, g := range groups {
		sum.HarnessHooks += len(g.Hooks)
		raw, err := textutil.MarshalNoEscape(g)
		if err != nil {
			return nil, err
		}
		raws = append(raws, raw)
	}
	return raws, nil
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
			if !IsHarness(cmd.Command) {
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

// WriteWithBackup copies to the cache-mirrored backup path (numbered on collision), then writes data atomically.
func WriteWithBackup(path string, data []byte) (backup string, err error) {
	return UpdateWithBackup(path, func([]byte) ([]byte, error) { return data, nil })
}

// UpdateWithBackup holds the file lock while it reads path (nil if absent), backs it up and writes merge(old).
func UpdateWithBackup(path string, merge func(old []byte) ([]byte, error)) (backup string, err error) {
	err = paths.WithFileLock(path, func() error {
		mode := os.FileMode(0o644)
		old, rerr := os.ReadFile(path)
		switch {
		case rerr == nil:
			if fi, serr := os.Stat(path); serr == nil {
				mode = fi.Mode().Perm()
			}
			if backup, err = paths.Backup(path, old); err != nil {
				return fmt.Errorf("backup %s: %w", path, err)
			}
		case !os.IsNotExist(rerr):
			return fmt.Errorf("read %s: %w", path, rerr)
		}
		data, err := merge(old)
		if err != nil {
			return err
		}
		return paths.AtomicWrite(path, data, mode)
	})
	return backup, err
}

// object is a JSON object that remembers key order, so a merge leaves the user's layout intact.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(b []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.set(tok.(string), raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// marshal writes 2-space indented JSON without HTML escaping (hook commands carry & and >).
func (o *object) marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteString(",")
		}
		key, _ := textutil.MarshalNoEscape(k)
		buf.WriteString("\n  ")
		buf.Write(key)
		buf.WriteString(": ")
		if err := json.Indent(&buf, bytes.TrimSpace(o.vals[k]), "  ", "  "); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(o.keys) > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func rawList(items []json.RawMessage) json.RawMessage {
	parts := make([][]byte, len(items))
	for i, it := range items {
		parts[i] = bytes.TrimSpace(it)
	}
	return json.RawMessage("[" + string(bytes.Join(parts, []byte(","))) + "]")
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}
