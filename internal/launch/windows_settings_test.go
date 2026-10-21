package launch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/hooks"
)

// decodeObj decodes a JSON object into key->raw, for presence/absence and value assertions.
func decodeObj(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// rawEqual compares two JSON values by parsed structure, not byte layout.
func rawEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// TestStripWindowsOnly proves stripWindowsOnly drops hooks/statusLine and every Linux-path string
// (top-level and inside env), while keeping plain keys and permissions untouched.
func TestStripWindowsOnly(t *testing.T) {
	in := []byte(`{
		"theme": "auto",
		"statusLine": {"type": "command", "command": "status-script"},
		"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "orca stop"}]}]},
		"permissions": {"allow": ["Bash(ls:*)"]},
		"SOME_PATH_KEY": "/home/edotau/.claude/skills",
		"env": {
			"HARNESS_BIN": "/usr/local/bin/claude-code",
			"HOME_REF": "$HOME/.claude/bin",
			"TILDE_REF": "~/.claude/bin/claude-code",
			"PLAIN_VAR": "hello"
		}
	}`)
	out, err := stripWindowsOnly(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	for _, k := range []string{"statusLine", "hooks", "SOME_PATH_KEY"} {
		if _, ok := m[k]; ok {
			t.Errorf("stripWindowsOnly kept %q, want dropped", k)
		}
	}
	if v, ok := m["theme"]; !ok || !rawEqual(t, v, json.RawMessage(`"auto"`)) {
		t.Errorf("theme = %s, want kept as \"auto\"", v)
	}
	perms, ok := m["permissions"]
	if !ok {
		t.Fatal("permissions dropped, want kept")
	}
	if want := []byte(`{"allow": ["Bash(ls:*)"]}`); !rawEqual(t, perms, want) {
		t.Errorf("permissions = %s, want %s", perms, want)
	}
	envRaw, ok := m["env"]
	if !ok {
		t.Fatal("env dropped, want kept (filtered)")
	}
	env := decodeObj(t, envRaw)
	for _, k := range []string{"HARNESS_BIN", "HOME_REF", "TILDE_REF"} {
		if _, ok := env[k]; ok {
			t.Errorf("env.%s kept, want dropped as a Linux path", k)
		}
	}
	if v, ok := env["PLAIN_VAR"]; !ok || !rawEqual(t, v, json.RawMessage(`"hello"`)) {
		t.Errorf("env.PLAIN_VAR = %s, want kept as \"hello\"", v)
	}
}

// TestStripWindowsOnlyKeepsNonPathStrings proves the marker match is substring-specific: a plain
// string that merely looks unix-ish (no marker substring) survives, so the strip isn't over-eager.
func TestStripWindowsOnlyKeepsNonPathStrings(t *testing.T) {
	in := []byte(`{"NOTE": "/opt/foo/bar", "COUNT": "3"}`)
	out, err := stripWindowsOnly(in)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, out)
	if v, ok := m["NOTE"]; !ok || !rawEqual(t, v, json.RawMessage(`"/opt/foo/bar"`)) {
		t.Errorf("NOTE = %s, want kept (no marker matched)", v)
	}
	if _, ok := m["COUNT"]; !ok {
		t.Error("COUNT dropped, want kept")
	}
}

// winHermetic isolates config/vault (hermetic) and points the Windows profile at a fresh temp dir,
// so these tests never touch the real Windows profile.
func winHermetic(t *testing.T) string {
	t.Helper()
	hermetic(t, "")
	profile := t.TempDir()
	t.Setenv(windowsProfileEnv, profile)
	return profile
}

func winSettingsPath(profile string) string {
	return filepath.Join(profile, ".claude", "settings.json")
}

// TestInstallWindowsSettings_MergeKeepsForeign proves Merge preserves a foreign `remote` key and the
// user's own statusLine (harness's statusLine is stripped from the Windows render, so it never owns
// that key), while still replacing an owned plain key (theme) with the rendered value.
func TestInstallWindowsSettings_MergeKeepsForeign(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"remote":{"defaultEnvironmentId":"abc"},"statusLine":{"type":"command","command":"my-custom-status"},"theme":"light"}`
	if err := os.WriteFile(file, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := InstallWindowsSettings(&out, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, b)
	if v, ok := m["remote"]; !ok || !rawEqual(t, v, json.RawMessage(`{"defaultEnvironmentId":"abc"}`)) {
		t.Errorf("remote = %s, want kept foreign key unchanged", v)
	}
	if v, ok := m["statusLine"]; !ok || !rawEqual(t, v, json.RawMessage(`{"type":"command","command":"my-custom-status"}`)) {
		t.Errorf("statusLine = %s, want the user's own kept (harness statusLine never reaches the Windows file)", v)
	}
	if _, ok := m["hooks"]; ok {
		t.Error("hooks present in Windows settings.json, want absent (no hook runner on Windows)")
	}
	if v, ok := m["theme"]; !ok || !rawEqual(t, v, json.RawMessage(`"auto"`)) {
		t.Errorf("theme = %s, want replaced by the owned rendered value \"auto\"", v)
	}
	if !bytes.Contains(out.Bytes(), []byte("settings (windows)")) {
		t.Errorf("output %q missing the settings (windows) banner", out.String())
	}
}

// TestInstallWindowsSettings_BackupCreated proves a changed write leaves a .bak-* alongside the file.
func TestInstallWindowsSettings_BackupCreated(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"theme":"light"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := InstallWindowsSettings(&out, false); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(file + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Error("no backup file created for a changed write")
	}
}

// TestInstallWindowsSettings_BOMPreserved proves a UTF-8 BOM on the existing file survives the write,
// and a file with none in gets none out.
func TestInstallWindowsSettings_BOMPreserved(t *testing.T) {
	t.Run("BOM in -> BOM out", func(t *testing.T) {
		profile := winHermetic(t)
		file := winSettingsPath(profile)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"theme":"light"}`)...)
		if err := os.WriteFile(file, withBOM, 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := InstallWindowsSettings(&out, false); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
			t.Error("BOM dropped on write, want preserved")
		}
	})
	t.Run("no BOM in -> no BOM out", func(t *testing.T) {
		profile := winHermetic(t)
		file := winSettingsPath(profile)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(`{"theme":"light"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := InstallWindowsSettings(&out, false); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
			t.Error("BOM added on write, want none (existing file carried none)")
		}
	})
}

// TestCheckWindowsSettingsDrift_Clean proves a just-installed Windows settings.json reports no drift.
func TestCheckWindowsSettingsDrift_Clean(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	var installOut bytes.Buffer
	if err := InstallWindowsSettings(&installOut, false); err != nil {
		t.Fatal(err)
	}
	drift, ok, err := CheckWindowsSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true (profile resolved via HARNESS_WINDOWS_PROFILE)")
	}
	if drift.Drifted() {
		t.Errorf("Drifted() = true right after install, want false; lines: %v", drift.LinesLabeled("settings (windows)"))
	}
}

// TestCheckWindowsSettingsDrift_HandEdited proves a hand-edited owned key (theme) surfaces as a
// labeled "settings (windows)" drift line and fails the check.
func TestCheckWindowsSettingsDrift_HandEdited(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	var installOut bytes.Buffer
	if err := InstallWindowsSettings(&installOut, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeObj(t, b)
	m["theme"] = json.RawMessage(`"dark"`)
	edited, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	drift, ok, err := CheckWindowsSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !drift.Drifted() {
		t.Error("Drifted() = false after a hand-edited theme, want true")
	}
	var found bool
	for _, l := range drift.LinesLabeled("settings (windows)") {
		if l == "settings (windows): theme differs from the template" {
			found = true
		}
	}
	if !found {
		t.Errorf("lines %v missing the theme drift line", drift.LinesLabeled("settings (windows)"))
	}
}

// brokenCmdPATH points PATH at an empty dir so `cmd.exe` can never be found, without touching the
// real Windows profile or filesystem; it leaves HARNESS_WINDOWS_PROFILE unset so resolution falls
// through to the real cmd.exe lookup, which then fails safely.
func brokenCmdPATH(t *testing.T) {
	t.Helper()
	t.Setenv(windowsProfileEnv, "")
	t.Setenv("PATH", t.TempDir())
}

// TestCheckWindowsSettingsDrift_MissingProfile proves an unresolvable profile reports ok=false,
// nil error, and no drift lines — a skip, not a failure.
func TestCheckWindowsSettingsDrift_MissingProfile(t *testing.T) {
	hermetic(t, "")
	brokenCmdPATH(t)
	drift, ok, err := CheckWindowsSettingsDrift()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Fatal("ok = true, want false (profile unresolvable)")
	}
	if len(drift.LinesLabeled("settings (windows)")) != 0 {
		t.Errorf("lines %v, want none when the profile is unresolved", drift.LinesLabeled("settings (windows)"))
	}
}

// TestInstallWindowsSettings_MissingProfileSkip proves InstallWindowsSettings prints a one-line skip
// note and returns nil, never an error, when the profile can't be resolved.
func TestInstallWindowsSettings_MissingProfileSkip(t *testing.T) {
	if !hooks.OnWSL() {
		t.Skip("off WSL InstallWindowsSettings is a silent no-op (TestInstallWindowsSettings_NonWSLNoop covers it)")
	}
	hermetic(t, "")
	brokenCmdPATH(t)
	var out bytes.Buffer
	if err := InstallWindowsSettings(&out, false); err != nil {
		t.Fatalf("err = %v, want nil (skip, not failure)", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("skip")) {
		t.Errorf("output %q missing a skip note", out.String())
	}
}

// TestInstallWindowsSettings_NonWSLNoop proves InstallWindowsSettings is silent off WSL. This dev box
// is itself WSL (no seam exists to fake hooks.OnWSL() false), so the assertion only runs on a genuine
// non-WSL host; elsewhere it documents why it skips rather than asserting something unexercised.
func TestInstallWindowsSettings_NonWSLNoop(t *testing.T) {
	hermetic(t, "")
	t.Setenv(windowsProfileEnv, "")
	if hooks.OnWSL() {
		t.Skip("host is WSL; the off-WSL no-op branch has no test seam and only runs on a non-WSL host")
	}
	var out bytes.Buffer
	if err := InstallWindowsSettings(&out, false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("non-WSL must no-op silently, got %q", out.String())
	}
}

// TestCheckWindowsSettingsDrift_ForeignKeptNotDrift: a Windows-only key (Orca's hooks, remote) is reported as
// kept, never as drift, so `generate --check` stays green while install preserves it.
func TestCheckWindowsSettingsDrift_ForeignKeptNotDrift(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"remote":{"defaultEnvironmentId":"env_x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var installOut bytes.Buffer
	if err := InstallWindowsSettings(&installOut, false); err != nil {
		t.Fatal(err)
	}
	drift, _, err := CheckWindowsSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if drift.Drifted() || len(drift.Foreign) != 0 {
		t.Errorf("Drifted() = %v, Foreign = %v; want a kept key, not drift", drift.Drifted(), drift.Foreign)
	}
	want := "settings (windows): remote is kept (not in the template)"
	if lines := drift.LinesLabeled("settings (windows)"); !slices.Contains(lines, want) {
		t.Errorf("lines = %v, want %q", lines, want)
	}
}

// TestCheckWindowsSettingsDrift_BOMIsNotInvalid: install keeps a UTF-8 BOM, so the drift check must read past it.
func TestCheckWindowsSettingsDrift_BOMIsNotInvalid(t *testing.T) {
	profile := winHermetic(t)
	file := winSettingsPath(profile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append([]byte{0xEF, 0xBB, 0xBF}, `{"theme":"light"}`...), 0o600); err != nil {
		t.Fatal(err)
	}
	var installOut bytes.Buffer
	if err := InstallWindowsSettings(&installOut, false); err != nil {
		t.Fatal(err)
	}
	drift, _, err := CheckWindowsSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if drift.Invalid != nil || drift.Drifted() {
		t.Errorf("after install of a BOM file: Invalid=%v Drifted=%v lines=%v", drift.Invalid, drift.Drifted(),
			drift.LinesLabeled("settings (windows)"))
	}
}
