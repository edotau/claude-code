package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexPreferencesApplyPreservesAndRestoresNativeConfig(t *testing.T) {
	vaulttest.Use(t)
	native := t.TempDir()
	t.Setenv("CODEX_HOME", native)
	path, target := filepath.Join(native, "config.toml"), filepath.Join(t.TempDir(), "tracked.toml")
	multiline := "instructions = '''\n" + codexSettingsBegin + "\nmodel_verbosity = 'high'\n" + codexSettingsEnd + "\n[not_a_table]\n'''\n"
	foreign := "# foreign preferences\nfixture_secret = 'private-fixture-config'\n" + multiline
	table := "[model_providers.foreign]\nname = 'my provider'\nbase_url = 'http://127.0.0.1:1'\nmodel_verbosity = 'high'\n"
	original := []byte("'model_verbosity' = 'high' # chosen before harness\n" + foreign + table)
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "low", "model_reasoning_effort": "medium"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), foreign) || !strings.Contains(string(after), table) {
		t.Fatal("foreign text changed")
	}
	if strings.Contains(string(after), "chosen before harness") {
		t.Fatal("duplicate root setting was kept")
	}
	values, err := ReadCodexSettings("")
	if err != nil || values["model_verbosity"] != "low" || values["model_reasoning_effort"] != "medium" {
		t.Fatalf("preferences not applied: %v, %v", values, err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("outgoing symlink replaced: %v", err)
	}
	b, found, err := LatestCodexBackup(path)
	if err != nil || !found || !bytes.Equal(b.Data, original) || !b.Existed {
		t.Fatalf("original snapshot missing: found=%v err=%v", found, err)
	}
	baseline, err := LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, after) {
		t.Fatalf("encrypted baseline missing: %v", err)
	}
	for _, p := range []string{path + ".backup", baselinePath("codex", path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("unexpected plaintext history at %s: %v", p, err)
		}
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	latest, found, err := LatestCodexBackup(path)
	if err != nil || !found || latest.Name != b.Name {
		t.Fatal("no-op apply added a snapshot")
	}
	if err := RevertConfigs(RenderOptions{Agents: []string{"codex"}, DryRun: true, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, after) {
		t.Fatalf("dry restore modified config: %v", err)
	}
	if err := RevertConfigs(RenderOptions{Agents: []string{"codex"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("restore did not recover exact original: %v", err)
	}
	if err := ApplyCodexSettings(""); err != nil {
		t.Fatal(err)
	}
	restored, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatalf("saved preferences undid restore: %v", err)
	}
}

func TestCodexPreferencesRejectUnsafeWrites(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("model_verbosity = 'high'\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if err := ApplyCodexSettings(home); err == nil || !strings.Contains(err.Error(), "encrypted backup failed") {
		t.Fatalf("write must fail without encrypted backup: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("backup failure overwrote config: %v", err)
	}
	if err := writeCodexConfig(path, []byte("stale file"), []byte("replacement"), true); err == nil || !strings.Contains(err.Error(), "changed before write") {
		t.Fatalf("stale write was not rejected before backup: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("stale write overwrote config: %v", err)
	}
}

func TestCodexPreferencesRestoreMissingOriginalAndOwnedValues(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	opts := RenderOptions{Home: home, Agents: []string{"codex"}, Out: io.Discard}
	if err := RevertConfigs(opts); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadCodexOverlay()
	if err != nil || ov["model_verbosity"] != "low" {
		t.Fatalf("restored preference was not synchronized: %v, %v", ov, err)
	}
	if err := RevertConfigs(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore should remove originally absent config: %v", err)
	}
}

func TestCodexPreferenceParserRejectsTrailingGarbage(t *testing.T) {
	for _, value := range []string{`"low" trailing`, `'low' trailing`, "`low`"} {
		if _, err := readCodexSettings("model_verbosity = " + value); err == nil {
			t.Fatalf("accepted unsupported string syntax %s", value)
		}
	}
	for _, value := range []string{`"low" # comment`, `'low' # comment`} {
		got, err := readCodexSettings("model_verbosity = " + value)
		if err != nil || got["model_verbosity"] != "low" {
			t.Fatalf("valid scalar rejected: %v", err)
		}
	}
}

func TestCodexPreferencesNoopWithoutTables(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = 'fixture'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, found, err := LatestCodexBackup(path)
	if err != nil || !found {
		t.Fatalf("snapshot missing: %v", err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("no-op apply changed config: %v", err)
	}
	next, found, err := LatestCodexBackup(path)
	if err != nil || !found || next.Name != b.Name {
		t.Fatal("no-op apply added a snapshot")
	}
	if drift, err := CodexSettingsDrift(home); err != nil || drift {
		t.Fatalf("applied preferences still report drift: %v", err)
	}
}

func TestCodexPreferencesQuotedKeyIdentity(t *testing.T) {
	foreign := "'\"model_verbosity\"' = 'foreign'\n"
	raw := foreign + "\"model\\u005fverbosity\" = 'high'\n"
	live, err := readCodexSettings(raw)
	if err != nil || live["model_verbosity"] != "high" {
		t.Fatalf("escaped native key was not decoded: %v, %v", live, err)
	}
	next, err := patchCodexSettings(raw, map[string]string{"model_verbosity": "low"})
	if err != nil || !strings.Contains(next, foreign) || strings.Contains(next, "'high'") {
		t.Fatalf("quoted key identity was lost: %v", err)
	}
}

func TestCodexRestoreMissingOriginalKeepsSymlinkTarget(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := SaveCodexOverlay(map[string]string{"model_verbosity": "low"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "tracked.toml")
	if err := os.WriteFile(target, []byte("model = 'keep'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := RevertConfigs(RenderOptions{Home: home, Agents: []string{"codex"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "model = 'keep'\n" {
		t.Fatalf("restore deleted the symlink target: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("restore left the config link behind: %v", err)
	}
}

func TestCodexLockIsSharedAcrossSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.toml")
	a, b := filepath.Join(dir, "a.toml"), filepath.Join(dir, "b.toml")
	if err := os.WriteFile(real, []byte("x = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{a, b} {
		if err := os.Symlink(real, l); err != nil {
			t.Fatal(err)
		}
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = withCodexLock(a, func() error { close(held); <-release; return nil })
	}()
	<-held
	go func() {
		_ = withCodexLock(b, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second symlink took a different lock while the first was held")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second symlink never acquired the lock")
	}
}

func TestCodexPreferenceParserAcceptsMultilineStrings(t *testing.T) {
	for _, value := range []string{"'''low'''", `"""low"""`, "'''\nlow'''", "\"\"\"\nlow\n\"\"\" # c"} {
		raw := "model_verbosity = " + value + "\nmodel = 'x'\n"
		got, err := readCodexSettings(raw)
		want := "low"
		if strings.Contains(value, "\nlow\n") {
			want = "low\n"
		}
		if err != nil || got["model_verbosity"] != want {
			t.Fatalf("%q: got %q, %v", value, got["model_verbosity"], err)
		}
	}
	next, err := patchCodexSettings("model_verbosity = '''high'''\n", map[string]string{"model_verbosity": "low"})
	if err != nil || strings.Contains(next, "high") || !strings.Contains(next, `model_verbosity = "low"`) {
		t.Fatalf("multiline native value not replaced in existing style: %q, %v", next, err)
	}
}

func TestCodexConfigCheck(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "config.toml")
	if err := os.WriteFile(file, []byte("PRIVATE-CONFIG-CONTENTS\nnot valid TOML"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		managed bool
		state   string
		detail  string
	}{
		{"absent", filepath.Join(root, "missing.toml"), false, "ok", ": absent"},
		{"user", file, false, "ok", ": present; CLI and machine settings may override it"},
		{"machine", file, true, "ok", ": present; machine policy may override CLI settings; effective route unverified"},
		{"nonregular", root, false, "warn", ": unavailable; effective route unknown"},
		{"unavailable", filepath.Join(file, "config.toml"), true, "warn", ": unavailable; effective route unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexConfigCheck("config check", tt.path, tt.managed)
			want := Check{Name: "config check", State: tt.state, Detail: tt.path + tt.detail}
			if got != want {
				t.Errorf("codexConfigCheck() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCodexConfigChecksPaths(t *testing.T) {
	root := t.TempDir()
	machine := filepath.Join(root, "managed.toml")
	requirements := filepath.Join(root, "requirements.toml")
	if err := os.WriteFile(machine, []byte("PRIVATE-MACHINE-CONTENTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := codexManagedPaths
	codexManagedPaths = func() []string { return []string{machine, requirements} }
	t.Cleanup(func() { codexManagedPaths = previous })
	userHome := filepath.Join(root, "user-home")
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	for _, override := range []string{"", filepath.Join(root, "custom-codex-home")} {
		name := "default home"
		if override != "" {
			name = "CODEX_HOME"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", override)
			home := override
			if home == "" {
				home = filepath.Join(userHome, ".codex")
			}
			config := filepath.Join(home, "config.toml")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte("PRIVATE-USER-CONTENTS"), 0o600); err != nil {
				t.Fatal(err)
			}
			got := CodexConfigChecks()
			want := []Check{
				{Name: "codex machine config", State: "ok", Detail: machine + ": present; machine policy may override CLI settings; effective route unverified"},
				{Name: "codex machine config", State: "ok", Detail: requirements + ": absent"},
				{Name: "codex user config", State: "ok", Detail: config + ": present; CLI and machine settings may override it"},
			}
			if len(got) != len(want) {
				t.Fatalf("got %d checks, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("check %d = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}
}

// codexRouteDetail/geminiRouteDetail are doctor's own route rows: names/booleans only (never a
// credential), unset means each vendor's own default decides.
func TestCodexRouteDetailShowsPinSource(t *testing.T) {
	hermetic(t, "") // scratch CLAUDE_CONFIG_DIR: SetPins below must never reach the real vault
	t.Setenv(providers.PinCodexModel, "")
	t.Setenv(providers.PinCodexEffort, "")
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "", providers.PinCodexEffort: ""}); err != nil {
		t.Fatal(err)
	}
	if got, want := codexRouteDetail(), "model=(default) source=default effort=default"; got != want {
		t.Errorf("codexRouteDetail(unset) = %q, want %q", got, want)
	}

	if err := providers.SetPins(map[string]string{providers.PinCodexModel: "gpt-6.1-sol", providers.PinCodexEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if got, want := codexRouteDetail(), "model=gpt-6.1-sol source=pin effort=high"; got != want {
		t.Errorf("codexRouteDetail(pinned) = %q, want %q", got, want)
	}
}

func TestGeminiRouteDetailShowsPinSource(t *testing.T) {
	hermetic(t, "") // scratch CLAUDE_CONFIG_DIR: SetPins below must never reach the real vault
	t.Setenv(providers.PinGeminiModel, "")
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: ""}); err != nil {
		t.Fatal(err)
	}
	if got, want := geminiRouteDetail(), "model=(default) source=default"; got != want {
		t.Errorf("geminiRouteDetail(unset) = %q, want %q", got, want)
	}

	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-2.5-pro"}); err != nil {
		t.Fatal(err)
	}
	if got, want := geminiRouteDetail(), "model=gemini-2.5-pro source=pin"; got != want {
		t.Errorf("geminiRouteDetail(pinned) = %q, want %q", got, want)
	}
}

func TestCodexNativeRoutingHooksStayVendorScoped(t *testing.T) {
	codex, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	claude, err := settings.SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	copilot, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(codex, &doc); err != nil {
		t.Fatal(err)
	}
	for name, event := range map[string]string{
		"codex-agent-route":    settings.EventPreToolUse,
		"codex-routing-result": settings.EventPostToolUse,
		"codex-routing-start":  settings.EventSessionStart,
		"codex-subagent-start": settings.EventSubagentStart,
	} {
		command := settings.AgentEnv + "=codex " + settings.HookBin + " hook " + event + " --" + name
		found := 0
		for actualEvent, groups := range doc.Hooks {
			for _, group := range groups {
				for _, hook := range group.Hooks {
					if hook.Command != command {
						continue
					}
					found++
					if actualEvent != event || hook.Timeout != 5 || hook.Async {
						t.Errorf("%s rendered under %s with handler %+v", name, actualEvent, hook)
					}
					if event == settings.EventPreToolUse || event == settings.EventPostToolUse {
						matcher, err := regexp.Compile(group.Matcher)
						if err != nil {
							t.Fatal(err)
						}
						for _, tool := range []string{"spawn_agent", "collaborationspawn_agent", "multi_agent_v1.spawn_agent"} {
							if !matcher.MatchString(tool) {
								t.Errorf("%s matcher misses %q", name, tool)
							}
						}
						if matcher.MatchString("spawn_agent_extra") {
							t.Errorf("%s matches unrelated tool", name)
						}
					} else if group.Matcher != "" {
						t.Errorf("%s lifecycle matcher = %q", name, group.Matcher)
					}
				}
			}
		}
		if found != 1 {
			t.Errorf("%s rendered %d times", name, found)
		}
		for vendor, body := range map[string][]byte{"claude": claude, "copilot": copilot} {
			if strings.Contains(string(body), "--"+name) {
				t.Errorf("%s hook leaked into %s", name, vendor)
			}
		}
	}
}

func TestCodexJSONVendorMarker(t *testing.T) {
	b, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, groups := range block.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				count++
				if !strings.HasPrefix(hook.Command, "HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook ") {
					t.Errorf("Codex command missing vendor marker: %q", hook.Command)
				}
				if !settings.IsHarness(hook.Command) {
					t.Errorf("Codex command not recognized as harness: %q", hook.Command)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no Codex commands rendered")
	}
}
