package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateVendorEnv sandboxes every credential/config surface vendorModelWord's registry lookup can
// touch (HOME, XDG dirs, CLAUDE_CONFIG_DIR, PATH, CODEX_HOME, and every credential/pin env var) so a
// test never depends on — or leaks into — the real machine's logins, keys, gh CLI, or OS keyring.
func isolateVendorEnv(t *testing.T) (home, cfg string) {
	t.Helper()
	home = t.TempDir()
	cfg = t.TempDir()
	for _, kv := range [][2]string{
		{"HOME", home}, {"USERPROFILE", home},
		{"XDG_CONFIG_HOME", filepath.Join(home, ".config")},
		{"XDG_DATA_HOME", filepath.Join(home, ".local", "share")},
		{"CLAUDE_CONFIG_DIR", cfg},
		{"PATH", t.TempDir()}, // empty: no real gh/codex/opencode binary is ever found on PATH
		{"CODEX_HOME", filepath.Join(home, ".codex")},
	} {
		t.Setenv(kv[0], kv[1])
	}
	// Credential/pin env vars: force every host value off for the duration of the test, deterministic
	// either way (present with a fake value, or cleared) rather than inheriting ambient machine state.
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN",
		"HARNESS_PROVIDER", "HARNESS_MODEL", "HARNESS_OPUS_MODEL", "HARNESS_SONNET_MODEL", "HARNESS_HAIKU_MODEL",
	} {
		t.Setenv(k, "")
	}
	return home, cfg
}

// writeProvidersOverlay drops a providers.json overlay into the isolated CLAUDE_CONFIG_DIR.
func writeProvidersOverlay(t *testing.T, cfg, overlay string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg, "providers.json"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- prefixDescription: pure function, no I/O, no env isolation needed ---

func TestPrefixDescriptionPure(t *testing.T) {
	cases := []struct {
		name, desc, word, want string
	}{
		{"empty desc untouched", "", "sonnet", ""},
		{"empty word untouched", "fix the typo", "", "fix the typo"},
		{"prefixes plainly", "fix the typo", "sonnet", "sonnet · fix the typo"},
		{"already prefixed by this hook is untouched", "sonnet · fix the typo", "haiku", "sonnet · fix the typo"},
		{"already prefixed by a DIFFERENT word is still untouched (shape, not exact match)",
			"gpt-6.1-sol · repair clean tests", "claude-sonnet-5", "gpt-6.1-sol · repair clean tests"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prefixDescription(c.desc, c.word); got != c.want {
				t.Errorf("prefixDescription(%q, %q) = %q, want %q", c.desc, c.word, got, c.want)
			}
		})
	}
}

func TestPrefixDescriptionLengthCap(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := prefixDescription(long, "claude-opus-5-5")
	if n := len([]rune(got)); n > descriptionCap {
		t.Fatalf("prefixed description is %d runes, want <= %d: %q", n, descriptionCap, got)
	}
	if !strings.HasPrefix(got, "claude-opus-5-5 · ") {
		t.Errorf("got %q, want it to start with the word prefix", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want a truncated body ending in the ellipsis", got)
	}
}

func TestPrefixDescriptionWordAloneExceedsCap(t *testing.T) {
	word := strings.Repeat("m", descriptionCap+10)
	got := prefixDescription("fix a typo", word)
	if got != word+" · " {
		t.Errorf("got %q, want the bare prefix %q (nothing left for the body)", got, word+" · ")
	}
}

// --- vendorModelWord: the route/model resolution seam, driven through the real registry+vendors
// packages (no injectable seam exists — see the handback report) with every credential surface
// hermetically isolated above so the result is deterministic and never touches the real keychain. ---

func TestVendorModelWordIgnoresNonVendorAgents(t *testing.T) {
	isolateVendorEnv(t)
	for _, name := range []string{"claude", "code-workers", "Explore", "", "general-purpose"} {
		if word, ok := vendorModelWord(name); ok {
			t.Errorf("vendorModelWord(%q) = (%q, true), want ok=false — not a vendor-routed agent", name, word)
		}
	}
}

func TestVendorModelWordResolvesPerVendor(t *testing.T) {
	t.Run("codex: chatgpt login present -> chatgpt's own opus model", func(t *testing.T) {
		home, _ := isolateVendorEnv(t)
		mustWriteFile(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"access_token":"x"}}`)
		if word, ok := vendorModelWord("codex"); !ok || word != "gpt-6-astra" {
			t.Errorf("vendorModelWord(codex) = (%q, %v), want (%q, true)", word, ok, "gpt-6-astra")
		}
	})
	t.Run("gemini: GEMINI_API_KEY present -> gemini's own opus model", func(t *testing.T) {
		isolateVendorEnv(t)
		t.Setenv("GEMINI_API_KEY", "fake-gemini-key")
		if word, ok := vendorModelWord("gemini"); !ok || word != "gemini-4-argon" {
			t.Errorf("vendorModelWord(gemini) = (%q, %v), want (%q, true)", word, ok, "gemini-4-argon")
		}
	})
	t.Run("copilot: GH_TOKEN present -> github-copilot's own opus model", func(t *testing.T) {
		isolateVendorEnv(t)
		t.Setenv("GH_TOKEN", "fake-gh-token")
		if word, ok := vendorModelWord("copilot"); !ok || word != "gpt-6.1-sol" {
			t.Errorf("vendorModelWord(copilot) = (%q, %v), want (%q, true)", word, ok, "gpt-6.1-sol")
		}
	})
	t.Run("opencode: own Copilot login present -> opencode's sonnet wire id", func(t *testing.T) {
		home, _ := isolateVendorEnv(t)
		mustWriteFile(t, filepath.Join(home, ".local", "share", "opencode", "auth.json"),
			`{"github-copilot":{"type":"oauth"}}`)
		if word, ok := vendorModelWord("opencode"); !ok || word != "claude-sonnet-5.5" {
			t.Errorf("vendorModelWord(opencode) = (%q, %v), want (%q, true)", word, ok, "claude-sonnet-5.5")
		}
	})
}

func TestVendorModelWordNoRouteOnSelectError(t *testing.T) {
	// Overlay the session default provider with no "opus" model: providers.Select errors (no opus
	// target), which is the one deterministic way to force a genuine no-route for codex/gemini —
	// ResolveTarget otherwise always falls back to the session's own default target (see report).
	overlay := `{"providers":{"subscription":{"kind":"anthropic","base_url":"https://api.anthropic.com",` +
		`"auth":{"type":"passthrough"},"models":{"sonnet":"x","haiku":"y"},"one_m":true}}}`
	for _, vendor := range []string{"codex", "gemini"} {
		t.Run(vendor, func(t *testing.T) {
			_, cfg := isolateVendorEnv(t)
			writeProvidersOverlay(t, cfg, overlay)
			if word, ok := vendorModelWord(vendor); !ok || word != "no-route" {
				t.Errorf("vendorModelWord(%s) = (%q, %v), want (%q, true)", vendor, word, ok, "no-route")
			}
		})
	}
}

// TestVendorModelWordCopilotOpencodeDeadRouteEmitsNoRoute: a dead copilot/opencode route prefixes "no-route".
func TestVendorModelWordCopilotOpencodeDeadRouteEmitsNoRoute(t *testing.T) {
	for _, vendor := range []string{"copilot", "opencode"} {
		t.Run(vendor, func(t *testing.T) {
			_, cfg := isolateVendorEnv(t)
			// No login env/file for this vendor, and no anthropic/gemini credential to fall back to.
			writeProvidersOverlay(t, cfg, `{"fallback":[],"providers":{"anthropic":null,"gemini":null}}`)
			word, ok := vendorModelWord(vendor)
			if !ok || word != "no-route" {
				t.Errorf("vendorModelWord(%s) = (%q, %v) — if this now reads (\"no-route\", true), "+
					"the Leg dead-branch bug has been fixed; update this test's expectation to match.", vendor, word, ok)
			}
		})
	}
}

func TestVendorModelWordFailsOpenOnLoadError(t *testing.T) {
	_, cfg := isolateVendorEnv(t)
	writeProvidersOverlay(t, cfg, `{not valid json`)
	for _, vendor := range []string{"codex", "gemini", "copilot", "opencode"} {
		if word, ok := vendorModelWord(vendor); ok || word != "" {
			t.Errorf("vendorModelWord(%s) on a broken registry = (%q, %v), want (\"\", false) — fail open", vendor, word, ok)
		}
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- AgentEffort end to end: the prefix applies ahead of, and independently from, the model pin ---

func TestAgentEffortPrefixesVendorDescriptionOnly(t *testing.T) {
	isolateVendorEnv(t)
	t.Setenv("GH_TOKEN", "fake-gh-token")
	in := `{"tool_name":"Agent","tool_input":{` +
		`"description":"repair clean tests",` +
		`"prompt":"repair the clean tests the rename broke",` +
		`"model":"opus",` + // explicit model: the model-pin section must stay untouched
		`"isolation":"worktree",` +
		`"run_in_background":true,` +
		`"subagent_type":"copilot"}}`
	got, code := runAgentEffort(t, in)
	if code != ExitProceed {
		t.Fatalf("exit code = %d, want ExitProceed", code)
	}
	if got == nil {
		t.Fatal("emitted nothing, want a prefixed description")
	}
	if got["description"] != "gpt-6.1-sol · repair clean tests" {
		t.Errorf("description = %v, want %q", got["description"], "gpt-6.1-sol · repair clean tests")
	}
	if got["model"] != "opus" {
		t.Errorf("model = %v, want untouched \"opus\" — the prefix must not bleed into the model-pin decision", got["model"])
	}
	for k, want := range map[string]any{
		"prompt": "repair the clean tests the rename broke", "isolation": "worktree",
		"run_in_background": true, "subagent_type": "copilot",
	} {
		if got[k] != want {
			t.Errorf("updatedInput[%q] = %v, want %v (original key not preserved)", k, got[k], want)
		}
	}
	if len(got) != 6 {
		t.Errorf("updatedInput has %d keys, want 6 (no key dropped or invented): %v", len(got), got)
	}
}

func TestAgentEffortVendorPrefixIdempotent(t *testing.T) {
	isolateVendorEnv(t)
	t.Setenv("GH_TOKEN", "fake-gh-token")
	in := `{"tool_name":"Agent","tool_input":{` +
		`"description":"gpt-6.1-sol · repair clean tests",` +
		`"model":"opus",` +
		`"subagent_type":"copilot"}}`
	got, code := runAgentEffort(t, in)
	if code != ExitProceed {
		t.Fatalf("exit code = %d, want ExitProceed", code)
	}
	if got != nil {
		t.Errorf("already-prefixed description was rewritten to %v, want the hook to emit nothing", got)
	}
}

func TestAgentEffortVendorPrefixLengthCapEndToEnd(t *testing.T) {
	isolateVendorEnv(t)
	t.Setenv("GEMINI_API_KEY", "fake-gemini-key")
	long := strings.Repeat("do the thing and then some more ", 10)
	in := `{"tool_name":"Agent","tool_input":{"description":"` + long + `","model":"opus","subagent_type":"gemini"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("emitted nothing, want a capped, prefixed description")
	}
	desc, _ := got["description"].(string)
	if n := len([]rune(desc)); n > descriptionCap {
		t.Errorf("description is %d runes, want <= %d: %q", n, descriptionCap, desc)
	}
	if !strings.HasPrefix(desc, "gemini-4-argon · ") {
		t.Errorf("description = %q, want it to start with the resolved model word", desc)
	}
}

func TestAgentEffortNonVendorAgentsUntouchedByPrefix(t *testing.T) {
	isolateVendorEnv(t)
	for _, subagent := range []string{"claude", "code-workers", "Explore"} {
		in := `{"tool_name":"Agent","tool_input":{"description":"fix a typo","model":"opus","subagent_type":"` + subagent + `"}}`
		got, code := runAgentEffort(t, in)
		if code != ExitProceed {
			t.Fatalf("%s: exit code = %d, want ExitProceed", subagent, code)
		}
		if got != nil {
			t.Errorf("%s: description was rewritten to %v, want untouched (not a vendor-routed agent)", subagent, got)
		}
	}
}

func TestAgentEffortVendorPrefixAndModelPinApplyTogetherIndependently(t *testing.T) {
	isolateVendorEnv(t)
	t.Setenv("GH_TOKEN", "fake-gh-token")
	// No explicit model, and "copilot" carries no roster frontmatter pin in the isolated CLAUDE_CONFIG_DIR
	// — both the prefix and the classifier's model-pin fire on the same dispatch.
	in := `{"tool_name":"Agent","tool_input":{` +
		`"description":"repair clean tests",` +
		`"prompt":"fix the typo in the help text",` +
		`"run_in_background":false,` +
		`"subagent_type":"copilot"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("emitted nothing, want both the prefix and the model pin applied")
	}
	if got["description"] != "gpt-6.1-sol · repair clean tests" {
		t.Errorf("description = %v, want the prefix applied", got["description"])
	}
	if got["model"] != "haiku" {
		t.Errorf("model = %v, want the classifier's haiku pin applied independently of the prefix", got["model"])
	}
	want := map[string]any{
		"description": "gpt-6.1-sol · repair clean tests", "prompt": "fix the typo in the help text",
		"run_in_background": false, "subagent_type": "copilot", "model": "haiku",
	}
	if len(got) != len(want) {
		t.Fatalf("updatedInput has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("updatedInput[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestAgentEffortFailsOpenOnRegistryLoadError(t *testing.T) {
	_, cfg := isolateVendorEnv(t)
	writeProvidersOverlay(t, cfg, `{not valid json`)
	in := `{"tool_name":"Agent","tool_input":{` +
		`"description":"repair clean tests",` +
		`"prompt":"fix the typo in the help text",` +
		`"subagent_type":"copilot"}}`
	got, code := runAgentEffort(t, in)
	if code != ExitProceed {
		t.Fatalf("exit code = %d, want ExitProceed — a broken registry must never block the spawn", code)
	}
	if got == nil {
		t.Fatal("emitted nothing, want the model-pin half to still apply independently of the broken route lookup")
	}
	if got["description"] != "repair clean tests" {
		t.Errorf("description = %v, want untouched (route lookup failed, fail open)", got["description"])
	}
	if got["model"] != "haiku" {
		t.Errorf("model = %v, want the classifier's pin still applied despite the broken registry", got["model"])
	}
}
