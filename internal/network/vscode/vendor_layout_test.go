package vscode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSyncDefaultTemplateRoutesTerminalProfiles(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	dest := filepath.Join(dir, "settings.json")
	claudeBin := filepath.Join(dir, "bin")
	result, err := Sync("", dest, dir, claudeBin, false)
	if err != nil || !result.Changed {
		t.Fatalf("Sync = %+v, err %v", result, err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if value, exists := settings["claudeCode.claudeProcessWrapper"]; exists {
		t.Errorf("rendered template has claudeProcessWrapper = %v", value)
	}
	if got, want := settings["chatgpt.cliExecutable"], claudeBin+"/codex"; got != want {
		t.Errorf("chatgpt.cliExecutable = %v, want %q", got, want)
	}
	for _, platform := range []string{"linux", "osx"} {
		t.Run(platform, func(t *testing.T) {
			key := "terminal.integrated.profiles." + platform
			profiles, ok := settings[key].(map[string]any)
			if !ok {
				t.Fatalf("missing profiles in %s", key)
			}
			for _, vendor := range []struct {
				profile string
				verb    string
			}{
				{"Claude", "claude"},
				{"Codex", "codex"},
				{"OpenCode", "opencode"},
				{"Gemini", "gemini"},
				{"Copilot", "copilot"},
			} {
				profile, ok := profiles[vendor.profile].(map[string]any)
				if !ok {
					t.Errorf("missing %s profile", vendor.profile)
					continue
				}
				if profile["path"] != "bash" {
					t.Errorf("%s path = %v, want bash", vendor.profile, profile["path"])
				}
				want := []any{"-lic", "exec " + claudeBin + "/claude-code " + vendor.verb}
				if !reflect.DeepEqual(profile["args"], want) {
					t.Errorf("%s args = %v, want %v", vendor.profile, profile["args"], want)
				}
			}
		})
	}
}

func TestSyncMigratesOnlyHarnessClaudeProcessWrapper(t *testing.T) {
	const wrapperKey = "claudeCode.claudeProcessWrapper"
	const claudeBin = "/opt/harness/bin"
	for _, tc := range []struct {
		name    string
		wrapper string
		remove  bool
	}{
		{"harness", claudeBin + "/claude", true},
		{"user", "/usr/local/bin/my-claude-wrapper", false},
		{"different-bin", "/opt/another-harness/bin/claude", false},
		{"with-arguments", claudeBin + "/claude --custom", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			tmpl := filepath.Join(dir, "template.json")
			dest := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(tmpl, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			original, err := json.Marshal(map[string]any{
				wrapperKey:   tc.wrapper,
				"local.only": true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, original, 0o600); err != nil {
				t.Fatal(err)
			}

			result, err := Sync(tmpl, dest, dir, claudeBin, true)
			if err != nil || result.Changed != tc.remove || result.Backup != "" {
				t.Fatalf("check = %+v, err %v; want Changed=%v and no backup", result, err, tc.remove)
			}
			body, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != string(original) {
				t.Fatal("--check rewrote settings")
			}

			result, err = Sync(tmpl, dest, dir, claudeBin, false)
			if err != nil || result.Changed != tc.remove {
				t.Fatalf("sync = %+v, err %v; want Changed=%v", result, err, tc.remove)
			}
			if tc.remove {
				saved, err := os.ReadFile(result.Backup)
				if err != nil {
					t.Fatal(err)
				}
				if string(saved) != string(original) {
					t.Fatal("backup did not preserve the original settings")
				}
			} else if result.Backup != "" {
				t.Errorf("unchanged user wrapper produced backup %q", result.Backup)
			}
			body, err = os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatal(err)
			}
			value, exists := settings[wrapperKey]
			if tc.remove {
				if exists {
					t.Errorf("harness wrapper survived migration: %v", value)
				}
			} else if !exists || value != tc.wrapper {
				t.Errorf("user wrapper = %v (exists %v), want %q", value, exists, tc.wrapper)
			}
			if settings["local.only"] != true {
				t.Error("migration did not preserve the unrelated setting")
			}
			result, err = Sync(tmpl, dest, dir, claudeBin, false)
			if err != nil || result.Changed || result.Backup != "" {
				t.Fatalf("idempotent sync = %+v, err %v", result, err)
			}
		})
	}
}
