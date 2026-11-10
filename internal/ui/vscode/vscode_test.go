package vscode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// hermeticVSCode isolates CLAUDE_CONFIG_DIR so Sync/SyncKeybindings backups never touch the real
// ~/.claude/state/harness.
func hermeticVSCode(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const jsoncOriginal = `{
  // local setting must survive
  "local.only": true,
  "terminal.integrated.copyOnSelection": false,
  "nested": {"local": 1,},
}`

// jsoncMergeFixture writes a template and a JSONC live settings file into one directory.
func jsoncMergeFixture(t *testing.T) (dir, tmpl, dest string) {
	t.Helper()
	hermeticVSCode(t)
	dir = t.TempDir()
	tmpl = filepath.Join(dir, "template.json")
	dest = filepath.Join(dir, "settings.json")
	writeFile(t, tmpl, `{
  "chatgpt.cliExecutable": "__CLAUDE_BIN__/codex",
  "terminal.integrated.copyOnSelection": true,
  "nested": {"template": true}
}`)
	writeFile(t, dest, jsoncOriginal)
	return dir, tmpl, dest
}

func TestSyncCheckLeavesJSONCUntouched(t *testing.T) {
	_, tmpl, dest := jsoncMergeFixture(t)
	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", true)
	if err != nil || !result.Changed || result.Backup != "" {
		t.Fatalf("check = %+v err %v", result, err)
	}
	if body, _ := os.ReadFile(dest); string(body) != jsoncOriginal {
		t.Fatal("--check rewrote Machine settings")
	}
}

func TestSyncMergesJSONCAndBacksUp(t *testing.T) {
	dir, tmpl, dest := jsoncMergeFixture(t)
	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err != nil || !result.Changed || result.Backup == "" {
		t.Fatalf("sync = %+v err %v", result, err)
	}
	body, _ := os.ReadFile(dest)
	for _, want := range []string{
		`"local.only": true`, `"template": true`, `"local": 1`,
		`/opt/claude/bin/codex`, `"terminal.integrated.copyOnSelection": true`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("merged settings missing %q:\n%s", want, body)
		}
	}
	if saved, _ := os.ReadFile(result.Backup); string(saved) != jsoncOriginal {
		t.Fatal("backup did not preserve the original JSONC")
	}
	if !strings.HasPrefix(result.Backup, filepath.Join(paths.CacheDir(), "backups")) {
		t.Errorf("backup %q, want it under CacheDir()/backups", result.Backup)
	}
	if siblings, _ := os.ReadDir(dir); len(siblings) != 2 {
		t.Errorf("files beside dest = %v, want only template.json and settings.json", siblings)
	}
}

// A second sync against the now-merged file has nothing left to change.
func TestSyncJSONCIsIdempotent(t *testing.T) {
	_, tmpl, dest := jsoncMergeFixture(t)
	if _, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false); err != nil {
		t.Fatal(err)
	}
	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err != nil || result.Changed {
		t.Fatalf("idempotent re-sync = %+v err %v", result, err)
	}
}

func vscodeRaceFixture(t *testing.T, live string) (tmpl, dest string) {
	t.Helper()
	dir := t.TempDir()
	tmpl = filepath.Join(dir, "template.json")
	dest = filepath.Join(dir, "settings.json")
	if err := os.WriteFile(tmpl, []byte(`{"terminal.integrated.copyOnSelection": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	return tmpl, dest
}

// Guards the read-modify-write window: VS Code does not honour our lock, so a save landing between
// our read and our write must abort the merge — never be overwritten, never be absent from the backup.
func TestSyncRefusesAStaleMerge(t *testing.T) {
	const original = `{"local.only": true}`
	const concurrent = `{"local.only": true, "saved.by.vscode": 42}`
	tmpl, dest := vscodeRaceFixture(t, original)

	BeforeWrite = func(string) {
		if err := os.WriteFile(dest, []byte(concurrent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { BeforeWrite = nil })

	result, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false)
	if err == nil {
		t.Errorf("stale merge was accepted: %+v", result)
	}
	if result.Changed {
		t.Error("stale merge reported changed")
	}
	if body, _ := os.ReadFile(dest); string(body) != concurrent {
		t.Errorf("concurrent save was overwritten:\n got %s\nwant %s", body, concurrent)
	}
	if result.Backup != "" {
		t.Errorf("backup %s written for a merge that must not proceed", result.Backup)
	}
}

// A write serializes on a lock in harness state (never beside VS Code's file); --check creates nothing.
func TestSyncCheckCreatesNoLockFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	tmpl, dest := vscodeRaceFixture(t, `{"local.only": true}`)
	lock := filepath.Join(paths.StateDir(), "vscode.lock")
	if _, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("--check created the lock (stat err %v)", err)
	}
	if _, err := Sync(tmpl, dest, "/opt/claude", "/opt/claude/bin", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("write path did not take the state lock: %v", err)
	}
	if _, err := os.Stat(dest + ".lock"); !os.IsNotExist(err) {
		t.Errorf("a lock was written beside VS Code's settings file")
	}
}

const userKeybindings = `// Place your key bindings in this file to override the defaults
[
  {"key": "ctrl+k ctrl+t", "command": "myExtension.doThing"}
]`

// readKeybindingEntries parses a (possibly JSONC) keybindings file.
func readKeybindingEntries(t *testing.T, path string) (entries []map[string]any, body []byte) {
	t.Helper()
	body, _ = os.ReadFile(path)
	if err := json.Unmarshal(textutil.StripJSONC(body), &entries); err != nil {
		t.Fatalf("merged keybindings did not parse: %v\n%s", err, body)
	}
	return entries, body
}

// User entries and a leading header comment survive a merge; template entries append in order.
func TestSyncKeybindingsAppendsTemplateEntriesPreservingUserEntryAndComment(t *testing.T) {
	tmpl, dest := keybindingsFixture(t, userKeybindings)
	dir := filepath.Dir(dest)
	result, err := SyncKeybindings(tmpl, dest, false)
	if err != nil || !result.Changed || result.Backup == "" {
		t.Fatalf("SyncKeybindings = %+v err %v", result, err)
	}
	entries, body := readKeybindingEntries(t, dest)
	if !strings.HasPrefix(string(body), "// Place your key bindings in this file to override the defaults") {
		t.Errorf("leading comment not preserved:\n%s", body)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (user + template):\n%s", len(entries), body)
	}
	if entries[0]["command"] != "myExtension.doThing" {
		t.Errorf("user entry was reordered: entries[0] = %v", entries[0])
	}
	if entries[1]["command"] != "workbench.action.terminal.newWithProfile" {
		t.Errorf("template entry not appended: entries[1] = %v", entries[1])
	}
	if !strings.HasPrefix(result.Backup, filepath.Join(paths.CacheDir(), "backups")) {
		t.Errorf("backup %q, want it under CacheDir()/backups", result.Backup)
	}
	if siblings, _ := os.ReadDir(dir); len(siblings) != 2 {
		t.Errorf("files beside dest = %v, want only keybindings.template.json and keybindings.json", siblings)
	}
}

// A '[' inside the header comment must not split it: the rewritten file has to stay parseable JSONC.
func TestSyncKeybindingsBracketInHeaderCommentStaysValid(t *testing.T) {
	user := "/* keep [these] bindings\n   as is */\n[\n  {\"key\": \"ctrl+k\", \"command\": \"myExtension.doThing\"}\n]\n"
	tmpl, dest := keybindingsFixture(t, user)
	if _, err := SyncKeybindings(tmpl, dest, false); err != nil {
		t.Fatal(err)
	}
	entries, body := readKeybindingEntries(t, dest)
	if len(entries) != 2 || !strings.HasPrefix(string(body), "/* keep [these] bindings\n   as is */\n[") {
		t.Errorf("header comment mangled (%d entries):\n%s", len(entries), body)
	}
}

// TestSyncKeybindingsOverlappingCommentDelimiterStaysValid: in `/*/ keep */` the first "*/" overlaps the opener.
func TestSyncKeybindingsOverlappingCommentDelimiterStaysValid(t *testing.T) {
	user := "/*/ keep */\n[\n  {\"key\": \"ctrl+k\", \"command\": \"myExtension.doThing\"}\n]\n"
	tmpl, dest := keybindingsFixture(t, user)
	if _, err := SyncKeybindings(tmpl, dest, false); err != nil {
		t.Fatal(err)
	}
	entries, body := readKeybindingEntries(t, dest)
	if len(entries) != 2 || !strings.HasPrefix(string(body), "/*/ keep */\n[") {
		t.Errorf("header comment mangled (%d entries):\n%s", len(entries), body)
	}
}

// A second run against the already-merged file has nothing left to change — no rewrite, no new backup.
func TestSyncKeybindingsIsIdempotent(t *testing.T) {
	tmpl, dest := keybindingsFixture(t, `[]`)
	if _, err := SyncKeybindings(tmpl, dest, false); err != nil {
		t.Fatal(err)
	}
	result, err := SyncKeybindings(tmpl, dest, false)
	if err != nil || result.Changed || result.Backup != "" {
		t.Fatalf("idempotent re-sync = %+v err %v", result, err)
	}
}

// A stale template entry — same command + profileName, different key — is replaced in place, not duplicated.
func TestSyncKeybindingsReplacesStaleEntryByCommandAndProfile(t *testing.T) {
	hermeticVSCode(t)
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "keybindings.template.json")
	dest := filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(tmpl, []byte(`[
  {"key": "ctrl+alt+1", "command": "workbench.action.terminal.newWithProfile", "args": {"profileName": "Claude"}}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Stale: same command+profile, old key — a prior template version, before a key was renumbered.
	stale := `[
  {"key": "ctrl+shift+9", "command": "workbench.action.terminal.newWithProfile", "args": {"profileName": "Claude"}}
]`
	if err := os.WriteFile(dest, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := SyncKeybindings(tmpl, dest, false)
	if err != nil || !result.Changed {
		t.Fatalf("SyncKeybindings = %+v err %v", result, err)
	}
	var entries []map[string]any
	body, _ := os.ReadFile(dest)
	if err := json.Unmarshal(textutil.StripJSONC(body), &entries); err != nil {
		t.Fatalf("merged keybindings did not parse: %v\n%s", err, body)
	}
	if len(entries) != 1 {
		t.Fatalf("stale entry was duplicated instead of replaced: %d entries:\n%s", len(entries), body)
	}
	if entries[0]["key"] != "ctrl+alt+1" {
		t.Errorf("stale entry key not replaced: %v", entries[0])
	}
}

// --check reports the drift without writing dest or dropping a backup.
func TestSyncKeybindingsCheckReportsDriftWithoutWriting(t *testing.T) {
	tmpl, dest := keybindingsFixture(t, `[]`)
	original, _ := os.ReadFile(dest)

	result, err := SyncKeybindings(tmpl, dest, true)
	if err != nil || !result.Changed {
		t.Fatalf("--check = %+v err %v, want Changed=true", result, err)
	}
	if result.Backup != "" {
		t.Errorf("--check produced a backup: %s", result.Backup)
	}
	if body, _ := os.ReadFile(dest); string(body) != string(original) {
		t.Error("--check rewrote keybindings.json")
	}
}

// Guards drift between the two embedded templates: every keybinding's profileName must exist as a
// terminal.integrated.profiles.linux key in the settings template, or the terminal command 404s.
func TestKeybindingsTemplateProfilesExistInSettingsTemplate(t *testing.T) {
	var entries []map[string]any
	if err := json.Unmarshal(textutil.StripJSONC(defaultKeybindingsTemplate), &entries); err != nil {
		t.Fatalf("embedded keybindings template did not parse: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded keybindings template has no entries")
	}

	var settings map[string]any
	if err := json.Unmarshal(textutil.StripJSONC(defaultTemplate), &settings); err != nil {
		t.Fatalf("embedded settings template did not parse: %v", err)
	}
	profiles, ok := settings["terminal.integrated.profiles.linux"].(map[string]any)
	if !ok {
		t.Fatal("settings template missing terminal.integrated.profiles.linux")
	}

	for _, entry := range entries {
		args, _ := entry["args"].(map[string]any)
		profile, _ := args["profileName"].(string)
		if profile == "" {
			continue
		}
		if _, ok := profiles[profile]; !ok {
			t.Errorf("keybinding profileName %q has no matching terminal.integrated.profiles.linux entry", profile)
		}
	}
}

// A headless box with only ~/.vscode-server has no local client, so keybindings must be skipped —
// unlike settings, there is no Machine scope to fall back to.
func TestDefaultKeybindingsDestSkipsVSCodeServerOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("vscode-server-only skip path is linux-specific")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".vscode-server"), 0o755); err != nil {
		t.Fatal(err)
	}

	dest, skip, note, err := DefaultKeybindingsDest()
	if err != nil {
		t.Fatal(err)
	}
	if !skip {
		t.Fatalf("skip = false, want true (no local client, only ~/.vscode-server); dest = %q", dest)
	}
	if note == "" {
		t.Error("skip without a note explaining why")
	}
}

func keybindingsFixture(t *testing.T, live string) (tmpl, dest string) {
	t.Helper()
	hermeticVSCode(t)
	dir := t.TempDir()
	tmpl = filepath.Join(dir, "keybindings.template.json")
	dest = filepath.Join(dir, "keybindings.json")
	if err := os.WriteFile(tmpl, []byte(`[
  {"key": "ctrl+alt+1", "command": "workbench.action.terminal.newWithProfile", "args": {"profileName": "Claude"}}
]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	return tmpl, dest
}

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
			checkTerminalProfiles(t, settings, "terminal.integrated.profiles."+platform, claudeBin)
		})
	}
}

// checkTerminalProfiles asserts every vendor profile under key launches its claude-code verb via bash.
func checkTerminalProfiles(t *testing.T, settings map[string]any, key, claudeBin string) {
	t.Helper()
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
}

const wrapperKey = "claudeCode.claudeProcessWrapper"

type wrapperMigration struct {
	dir, tmpl, dest string
	original        []byte
	claudeBin       string
	wrapper         string
	remove          bool
}

// checkNoRewrite runs --check and asserts it reports the drift but leaves dest untouched.
func (m wrapperMigration) checkNoRewrite(t *testing.T) {
	t.Helper()
	result, err := Sync(m.tmpl, m.dest, m.dir, m.claudeBin, true)
	if err != nil || result.Changed != m.remove || result.Backup != "" {
		t.Fatalf("check = %+v, err %v; want Changed=%v and no backup", result, err, m.remove)
	}
	body, err := os.ReadFile(m.dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(m.original) {
		t.Fatal("--check rewrote settings")
	}
}

// checkBackup asserts a migration backs up the original settings and an untouched wrapper makes no backup.
func (m wrapperMigration) checkBackup(t *testing.T, result Result) {
	t.Helper()
	if !m.remove {
		if result.Backup != "" {
			t.Errorf("unchanged user wrapper produced backup %q", result.Backup)
		}
		return
	}
	saved, err := os.ReadFile(result.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(m.original) {
		t.Fatal("backup did not preserve the original settings")
	}
}

// checkMigrated asserts the wrapper key was removed (harness) or kept (user) and local.only survived.
func (m wrapperMigration) checkMigrated(t *testing.T) {
	t.Helper()
	body, err := os.ReadFile(m.dest)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	value, exists := settings[wrapperKey]
	if m.remove {
		if exists {
			t.Errorf("harness wrapper survived migration: %v", value)
		}
	} else if !exists || value != m.wrapper {
		t.Errorf("user wrapper = %v (exists %v), want %q", value, exists, m.wrapper)
	}
	if settings["local.only"] != true {
		t.Error("migration did not preserve the unrelated setting")
	}
}

func newWrapperMigration(t *testing.T, claudeBin, wrapper string, remove bool) wrapperMigration {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	m := wrapperMigration{
		dir: dir, tmpl: filepath.Join(dir, "template.json"), dest: filepath.Join(dir, "settings.json"),
		claudeBin: claudeBin, wrapper: wrapper, remove: remove,
	}
	writeFile(t, m.tmpl, `{}`)
	original, err := json.Marshal(map[string]any{
		wrapperKey:   wrapper,
		"local.only": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.original = original
	writeFile(t, m.dest, string(original))
	return m
}

func TestSyncMigratesOnlyHarnessClaudeProcessWrapper(t *testing.T) {
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
			m := newWrapperMigration(t, claudeBin, tc.wrapper, tc.remove)
			m.checkNoRewrite(t)

			result, err := Sync(m.tmpl, m.dest, m.dir, claudeBin, false)
			if err != nil || result.Changed != tc.remove {
				t.Fatalf("sync = %+v, err %v; want Changed=%v", result, err, tc.remove)
			}
			m.checkBackup(t, result)
			m.checkMigrated(t)
			result, err = Sync(m.tmpl, m.dest, m.dir, claudeBin, false)
			if err != nil || result.Changed || result.Backup != "" {
				t.Fatalf("idempotent sync = %+v, err %v", result, err)
			}
		})
	}
}

func TestSyncEscapesTemplatePaths(t *testing.T) {
	hermeticVSCode(t)
	dir := t.TempDir()
	tmpl, dest := filepath.Join(dir, "template.json"), filepath.Join(dir, "settings.json")
	writeFile(t, tmpl, `{"home":"__CLAUDE_HOME__", "bin":"__CLAUDE_BIN__/codex"}`)
	home, bin := `/tmp/home"with\slash`, `/tmp/a"b/bin`
	if _, err := Sync(tmpl, dest, home, bin, false); err != nil {
		t.Fatalf("sync with quoted paths: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("merged JSON: %v", err)
	}
	if got["home"] != home || got["bin"] != bin+"/codex" {
		t.Fatalf("rendered paths = %v, want home %q and bin %q", got, home, bin+"/codex")
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
