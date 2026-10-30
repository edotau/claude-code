package configure

import (
	"maps"

	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/settings"
)

func codexPreferenceValue(key string, overlay, live map[string]string) (value, source string) {
	if value, ok := overlay[key]; ok {
		return value, "vault"
	}
	if value := live[key]; value != "" {
		return value, "native"
	}
	return "Codex default", "default"
}

func runCodexSettings(d *dialog) bool {
	ov, err := settings.LoadCodexOverlay()
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	live, err := generate.ReadCodexSettings(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	path, err := settings.CodexConfigPath(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("codex settings: %s\n", path)
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return runCodexSettingsMenu(d, ov, live)
	}
	drift, err := generate.CodexSettingsDrift(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	for _, field := range settings.CodexFields() {
		value, source := codexPreferenceValue(field.Key, ov, live)
		if saved, exists := ov[field.Key]; exists {
			value, source = saved, "vault"
			if live[field.Key] != saved {
				source += " · live differs"
			}
		}
		d.printf("%s  %s  %s\n", field.Key, value, source)
	}
	if drift {
		d.println("codex: saved preferences differ from native config")
	}
	return !d.o.Check || !drift
}

// runCodexSettingsMenu stages choices; only the explicit Save row writes or applies them.
func runCodexSettingsMenu(d *dialog, ov, live map[string]string) bool {
	next := maps.Clone(ov)
	fields := settings.CodexFields()
	for {
		rows := make([][2]string, 0, len(fields)+1)
		for _, f := range fields {
			value, source := codexPreferenceValue(f.Key, next, live)
			if _, existed := ov[f.Key]; existed {
				if _, kept := next[f.Key]; !kept {
					value, source = "Codex default", "after save"
				}
			}
			rows = append(rows, [2]string{f.Key, value + " · " + source})
		}
		rows = append(rows, [2]string{"Save and apply", "Codex only"})
		idx, err := pickPlain(d, "Codex settings", rows)
		if err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
		if idx < 0 {
			return true
		}
		if idx == len(fields) {
			return saveCodexSettings(d, ov, next)
		}
		f := fields[idx]
		choices := make([][2]string, 0, len(f.Values)+1)
		for _, value := range f.Values {
			choices = append(choices, [2]string{value, ""})
		}
		choices = append(choices, [2]string{"Codex default", "remove harness override"})
		choice, err := pickPlain(d, f.Key, choices)
		if err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
		if choice < 0 {
			continue
		}
		if choice == len(f.Values) {
			delete(next, f.Key)
		} else {
			next[f.Key] = f.Values[choice]
		}
	}
}

func saveCodexSettings(d *dialog, old, next map[string]string) bool {
	err := settings.UpdateCodexOverlay(func(current map[string]string) map[string]string {
		for _, f := range settings.CodexFields() {
			before, hadBefore := old[f.Key]
			after, hasAfter := next[f.Key]
			if hadBefore == hasAfter && before == after {
				continue
			}
			if hasAfter {
				current[f.Key] = after
			} else {
				delete(current, f.Key)
			}
		}
		return current
	})
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	if err := generate.ApplyCodexSettings(d.o.Home); err != nil {
		d.printf("codex: preferences saved; native apply failed: %v\n", err)
		return false
	}
	d.println("codex: preferences saved and applied")
	return true
}
