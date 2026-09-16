// Package settings renders the harness settings.json and merges it into an existing one without
// dropping foreign keys or foreign hooks.
package settings

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
)

//go:embed settings.tmpl.json
var tmpl string

// Render returns the harness settings: template with ${HOOK_BIN} expanded plus the hookspec hooks block.
func Render() ([]byte, error) {
	o, err := parseObject([]byte(strings.ReplaceAll(tmpl, "${HOOK_BIN}", hookspec.HookBin)))
	if err != nil {
		return nil, fmt.Errorf("settings template: %w", err)
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
	Owned        []string // template keys written (env/permissions unioned, others replaced)
	ForeignHooks int      // non-harness hook commands preserved
	HarnessHooks int
}

// Merge overlays rendered harness settings onto existing: foreign keys kept in place, env and permission
// lists unioned, harness hooks replaced, foreign hooks kept after them.
func Merge(existing, rendered []byte) ([]byte, Summary, error) {
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
	for _, k := range ex.keys {
		if _, owned := tp.vals[k]; !owned {
			sum.Kept = append(sum.Kept, k)
		}
	}
	for _, k := range tp.keys {
		sum.Owned = append(sum.Owned, k)
		v := tp.vals[k]
		var merged json.RawMessage
		switch k {
		case "env":
			merged, err = mergeObject(ex.vals[k], v)
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
			if !contains(have, r) {
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
		var groups []json.RawMessage
		if err := json.Unmarshal(b.vals[ev], &groups); err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", ev, err)
		}
		for _, raw := range groups {
			g, err := parseObject(raw)
			if err != nil {
				return nil, fmt.Errorf("hooks.%s: %w", ev, err)
			}
			var hooks, keep []json.RawMessage
			_ = json.Unmarshal(g.vals["hooks"], &hooks)
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
			events[ev] = append(events[ev], raw)
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

func rawList(items []json.RawMessage) json.RawMessage {
	parts := make([][]byte, len(items))
	for i, it := range items {
		parts[i] = bytes.TrimSpace(it)
	}
	return json.RawMessage("[" + string(bytes.Join(parts, []byte(","))) + "]")
}

// WriteWithBackup copies an existing file to <path>.bak-<timestamp>, then writes data atomically.
func WriteWithBackup(path string, data []byte) (backup string, err error) {
	mode := os.FileMode(0o644)
	if old, rerr := os.ReadFile(path); rerr == nil {
		if fi, serr := os.Stat(path); serr == nil {
			mode = fi.Mode().Perm()
		}
		backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return "", fmt.Errorf("backup %s: %w", backup, err)
		}
	}
	return backup, paths.AtomicWrite(path, data, mode)
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
