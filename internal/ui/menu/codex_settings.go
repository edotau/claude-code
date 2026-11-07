package menu

import (
	"maps"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/paths"
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
	ov, live, path, err := loadCodexSettings(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("codex settings: %s\n", path)
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return runCodexSettingsMenu(d, ov, live)
	}
	return reportCodexSettings(d, ov, live)
}

// loadCodexSettings reads the vault overlay, the native settings under home, and the native config path.
func loadCodexSettings(home string) (ov, live map[string]string, path string, err error) {
	if ov, err = codex.LoadCodexOverlay(); err != nil {
		return nil, nil, "", err
	}
	if live, err = codex.ReadCodexSettings(home); err != nil {
		return nil, nil, "", err
	}
	if path, err = paths.CodexConfigPath(home); err != nil {
		return nil, nil, "", err
	}
	return ov, live, path, nil
}

// reportCodexSettings prints one line per field (value, source) with no prompt or write; false on a drift in --check.
func reportCodexSettings(d *dialog, ov, live map[string]string) bool {
	drift, err := codex.CodexSettingsDrift(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	for _, field := range codex.CodexFields() {
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
	fields := codex.CodexFields()
	for {
		rows := codexSettingRows(fields, ov, next, live)
		idx, err := d.shell().PickPlain("Codex settings", rows)
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
		if err := stageCodexChoice(d, fields[idx], next); err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
	}
}

// codexSettingRows is one "<key> → value · source" row per field plus the trailing Save row.
func codexSettingRows(fields []codex.CodexField, ov, next, live map[string]string) [][2]string {
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
	return append(rows, [2]string{"Save and apply", "Codex only"})
}

// stageCodexChoice asks for f's value and stages it in next ("Codex default" drops it); a cancel stages nothing.
func stageCodexChoice(d *dialog, f codex.CodexField, next map[string]string) error {
	choices := make([][2]string, 0, len(f.Values)+1)
	for _, value := range f.Values {
		choices = append(choices, [2]string{value, ""})
	}
	choices = append(choices, [2]string{"Codex default", "remove harness override"})
	choice, err := d.shell().Pick(f.Key, choices)
	if err != nil || choice < 0 {
		return err
	}
	if choice == len(f.Values) {
		delete(next, f.Key)
	} else {
		next[f.Key] = f.Values[choice]
	}
	return nil
}

func saveCodexSettings(d *dialog, old, next map[string]string) bool {
	saved, err := configure.SaveCodexSettings(d.o.Home, old, next)
	if !saved {
		d.printf("codex: %v\n", err)
		return false
	}
	if err != nil {
		d.printf("codex: preferences saved; native apply failed: %v\n", err)
		return false
	}
	d.println("codex: preferences saved and applied")
	return true
}
