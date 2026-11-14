package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// errMatches reports err against want: "" wants nil, anything else an error containing it.
func errMatches(err error, want string) bool {
	if want == "" {
		return err == nil
	}
	return err != nil && strings.Contains(err.Error(), want)
}

// fakeKey is credential-shaped by prefix and length only.
var fakeKey = "sk-" + strings.Repeat("a", 24)

// D2: every agent × valid effort, unknown effort, and a field only another agent can use.
func TestValidateRecord(t *testing.T) {
	vaulttest.Use(t)
	cases := []struct {
		agent   string
		rec     vault.AgentRecord
		wantErr string // "" = accepted
	}{
		{"claude", vault.AgentRecord{Effort: "max", Aliases: map[string]string{"opus": "x"}}, ""},
		{"codex", vault.AgentRecord{Effort: "minimal"}, ""},
		{"codex", vault.AgentRecord{Effort: "max"}, ""}, // clamped at launch, reported by EffortClamp
		{"copilot", vault.AgentRecord{Effort: "high", Context: "1m", Account: "octo"}, ""},
		{"gemini", vault.AgentRecord{Effort: "xhigh"}, ""},
		{"opencode", vault.AgentRecord{Effort: "low"}, ""},
		{"claude", vault.AgentRecord{Effort: "minimal"}, `effort "minimal" for claude`},
		{"codex", vault.AgentRecord{Effort: "turbo"}, `effort "turbo" for codex`},
		{"copilot", vault.AgentRecord{Effort: "turbo"}, `effort "turbo" for copilot`},
		{"gemini", vault.AgentRecord{Effort: "turbo"}, `effort "turbo" for gemini`},
		{"opencode", vault.AgentRecord{Effort: "turbo"}, `effort "turbo" for opencode`},
		{"claude", vault.AgentRecord{Context: "1m"}, "claude: context is copilot only"},
		{"codex", vault.AgentRecord{Aliases: map[string]string{"opus": "x"}}, "codex: aliases is claude only"},
		{"copilot", vault.AgentRecord{Aliases: map[string]string{"opus": "x"}}, "copilot: aliases is claude only"},
		{"gemini", vault.AgentRecord{Account: "octo"}, "gemini: account is copilot only"},
		{"opencode", vault.AgentRecord{Extras: map[string]string{"k": "v"}}, ""}, // no CLI.Extras declared: not checked yet
		{"codex", vault.AgentRecord{Permissions: &vault.AgentPolicy{Mode: "yolo"}}, `unknown permission mode "yolo"`},
		{"codex", vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"OPENAI_API_KEY=x"}}}, "codex launch.env:"},
		{"codex", vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"--api-key", fakeKey}}}, "codex launch.args[1]: looks like"},
		{"codex", vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"--api-key=" + fakeKey}}}, "codex launch.args[0]: looks like"},
		{"copilot", vault.AgentRecord{Launch: &vault.AgentLaunch{Args: []string{"--allow-all", "--api-key", "$KEY"}}}, ""},
		{"nobody", vault.AgentRecord{}, `unknown agent "nobody"`},
	}
	for _, c := range cases {
		if err := ValidateRecord(c.agent, c.rec); !errMatches(err, c.wantErr) {
			t.Errorf("ValidateRecord(%s, %+v) = %v, want %q", c.agent, c.rec, err, c.wantErr)
		}
	}
}

// D2: codex clamps max to xhigh and says so; claude takes max as given.
func TestEffortClamp(t *testing.T) {
	for _, c := range [][3]string{{"codex", "max", "effort max→xhigh"}, {"claude", "max", ""}, {"codex", "high", ""}} {
		if got := EffortClamp(c[0], c[1]); got != c[2] {
			t.Errorf("EffortClamp(%s, %s) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// Declared extras keys take their listed values or "" (unset a built-in); free-form keys take any value.
func TestCheckExtras(t *testing.T) {
	decl := map[string][]string{"model_verbosity": {"low", "medium", "high"}, "free": nil}
	cases := []struct {
		extras  map[string]string
		wantErr string
	}{
		{map[string]string{"model_verbosity": "low"}, ""},
		{map[string]string{"model_verbosity": ""}, ""},
		{map[string]string{"free": "anything"}, ""},
		{map[string]string{"model_verbosity": "loud"}, `extras.model_verbosity "loud": want one of low, medium, high`},
		{map[string]string{"sandbox": "x"}, `extras key "sandbox" unsupported (want free, model_verbosity)`},
	}
	for _, c := range cases {
		if err := checkExtras("codex", decl, c.extras); !errMatches(err, c.wantErr) {
			t.Errorf("checkExtras(%v) = %v, want %q", c.extras, err, c.wantErr)
		}
	}
}

// settingsView is the fields the precedence tests compare, one line.
func settingsView(t *testing.T, agentName string, req RunOptions) string {
	t.Helper()
	s, err := ResolveSettings(agentName, req, providers.Target{Model: "m"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if s.Record.Launch != nil {
		args = s.Record.Launch.Args
	}
	p := s.Permissions
	return fmt.Sprintf("effort=%s args=%v mode=%s allow=%v deny=%v extras=%v tools=%v workdir=%s model=%s headless=%v",
		s.Effort, args, p.Mode, RuleStrings(p.Allow), RuleStrings(p.Deny), s.Record.Extras, s.Tools, s.WorkDir,
		s.Target.Model, s.Headless)
}

// storeCopilotRecord stores an effort, a launch block and a policy for copilot.
func storeCopilotRecord(t *testing.T) {
	t.Helper()
	err := vault.SetAgentRecord("copilot", vault.AgentRecord{Effort: "medium", Launch: &vault.AgentLaunch{Args: []string{"--banner"}},
		Permissions: &vault.AgentPolicy{Mode: "plan", Deny: []string{"Bash(rm:*)"}}})
	if err != nil {
		t.Fatal(err)
	}
}

// With no record, agents.json is the whole answer.
func TestResolveSettingsBuiltIn(t *testing.T) {
	vaulttest.Use(t)
	want := "effort= args=[--allow-all] mode= allow=[] deny=[] extras=map[defaultPermissionMode:allow-all] tools=false " +
		"workdir= model=m headless=true"
	if got := settingsView(t, "copilot", RunOptions{}); got != want {
		t.Errorf("built-in:\n got %s\nwant %s", got, want)
	}
}

// A read-only request resolves the emulated read-only rules even when its caller skipped checkRun, once.
func TestResolveSettingsReadOnlyRules(t *testing.T) {
	vaulttest.Use(t)
	req := RunOptions{Tools: true, ReadOnly: true}
	st, err := ResolveSettings("gemini", req, providers.Target{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(st.Permissions.Allow, ReadOnlyAllow) || !slices.Contains(st.Permissions.Deny, ReadOnlyDeny[0]) {
		t.Errorf("read-only gemini resolved %+v, want the read-only rules", st.Permissions)
	}
	req.Permissions = withReadOnlyRules(SchemaFor("gemini").CLI.Caps, Permissions{})
	again, _ := ResolveSettings("gemini", req, providers.Target{}, true)
	if len(again.Permissions.Allow) != len(st.Permissions.Allow) || len(again.Permissions.Deny) != len(st.Permissions.Deny) {
		t.Errorf("rules applied by checkRun were duplicated: %+v vs %+v", again.Permissions, st.Permissions)
	}
}

// The stored record replaces the built-in launch block and keeps the built-in extras it does not name.
func TestResolveSettingsRecordOverBuiltIn(t *testing.T) {
	vaulttest.Use(t)
	storeCopilotRecord(t)
	want := "effort=medium args=[--banner] mode=plan allow=[] deny=[Bash(rm:*)] extras=map[defaultPermissionMode:allow-all] " +
		"tools=false workdir= model=m headless=true"
	if got := settingsView(t, "copilot", RunOptions{}); got != want {
		t.Errorf("record over built-in:\n got %s\nwant %s", got, want)
	}
}

// The request's effort and mode win over the record; its rules add to the record's.
func TestResolveSettingsFlagOverRecord(t *testing.T) {
	vaulttest.Use(t)
	storeCopilotRecord(t)
	req := RunOptions{Effort: "high", Tools: true, WorkDir: "/w", Permissions: Permissions{Mode: "default", Allow: []Rule{{Tool: "Read"}}}}
	want := "effort=high args=[--banner] mode=default allow=[Read] deny=[Bash(rm:*)] extras=map[defaultPermissionMode:allow-all] " +
		"tools=true workdir=/w model=m headless=true"
	if got := settingsView(t, "copilot", req); got != want {
		t.Errorf("flag over record:\n got %s\nwant %s", got, want)
	}
}

// A bad record policy is an error whose Settings carry the built-in record: nothing of the stored one applies.
func TestResolveSettingsBadPolicyFallsBackToBuiltin(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Permissions: &vault.AgentPolicy{Deny: []string{"Bash("}},
		Launch: &vault.AgentLaunch{Env: []string{"FOO=1"}}}); err != nil {
		t.Fatal(err)
	}
	s, err := ResolveSettings("codex", RunOptions{}, providers.Target{}, false)
	if err == nil || s.Agent != "codex" || s.Record.Launch != nil || s.Record.Extras["approvalPolicy"] != "never" {
		t.Errorf("ResolveSettings = %+v, %v; want an error and the built-in codex record", s, err)
	}
}

// renderDefault is the agent driver's RenderSettings over route env ROUTE=r and argv [run]; the default never errs.
func renderDefault(t *testing.T, name string, headless bool) (env, argv []string) {
	t.Helper()
	st, err := ResolveSettings(name, RunOptions{}, providers.Target{}, headless)
	if err != nil {
		t.Fatal(err)
	}
	env, argv, emulated, cleanup, err := SchemaFor(name).Eng().RenderSettings(t.Context(), st, []string{"ROUTE=r"}, []string{"run"})
	if err != nil || cleanup == nil || emulated != nil {
		t.Fatalf("%s: emulated %v, cleanup nil %v, err %v; want none, a no-op cleanup, nil", name, emulated, cleanup == nil, err)
	}
	return env, argv
}

// The default renders exactly what ApplyLaunch did: launch.env under the route keys, launch.args interactive only.
func TestBaseRenderSettingsDefault(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"FOO=1", "ROUTE=launch"},
		Args: []string{"--x"}}}); err != nil {
		t.Fatal(err)
	}
	route, codexEnv := []string{"ROUTE=r"}, []string{"FOO=1", "ROUTE=r"}
	cases := []struct {
		name      string
		headless  bool
		env, argv []string
	}{
		{"claude", false, route, []string{"run"}},
		{"claude", true, route, []string{"run"}},
		{"codex", false, codexEnv, []string{"run", "--x"}},
		{"codex", true, codexEnv, []string{"run"}},
		{"copilot", false, route, []string{"run", "--allow-all"}},
		{"copilot", true, route, []string{"run"}},
		{"gemini", false, route, []string{"run", "--dangerously-skip-permissions"}},
		{"gemini", true, route, []string{"run"}},
		{"opencode", false, route, []string{"run"}},
		{"opencode", true, route, []string{"run"}},
	}
	for _, c := range cases {
		if env, argv := renderDefault(t, c.name, c.headless); !slices.Equal(env, c.env) || !slices.Equal(argv, c.argv) {
			t.Errorf("%s headless=%v: env %v argv %v; want env %v argv %v", c.name, c.headless, env, argv, c.env, c.argv)
		}
	}
}

// renderSpy is a codex driver that wires a route itself and records what RenderSettings and Exec see.
type renderSpy struct {
	fixtureDriver
	cleaned, rendered *int
	seen              *CLIPrep
}

func (renderSpy) Wire(context.Context, WireRoute, *Headless) ([]string, []string, error) {
	return []string{"ROUTE=1"}, []string{"exec"}, nil
}

// After marks the env so RenderSettings can show it ran second.
func (renderSpy) After(p *CLIPrep, _ RunOptions) { p.Env = append(p.Env, "AFTER=1") }

func (r renderSpy) RenderSettings(_ context.Context, s Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	*r.rendered++
	emulated := []string{"effort"}
	if slices.Contains(env, "AFTER=1") {
		emulated = append(emulated, "after-ran")
	}
	return env, append(argv, "--effort", s.Effort), emulated, func() { *r.cleaned++ }, nil
}

// runSpy dispatches req headless through a renderSpy registered as codex and returns what it counted and saw.
func runSpy(t *testing.T, req RunOptions) (cleaned, rendered int, seen CLIPrep) {
	t.Helper()
	spy := renderSpy{fixtureDriver{NewBaseEngine("codex")}, &cleaned, &rendered, &seen}
	registryMu.Lock()
	orig := drivers["codex"]
	drivers["codex"] = spy
	registryMu.Unlock()
	t.Cleanup(func() { registryMu.Lock(); drivers["codex"] = orig; registryMu.Unlock() })
	if _, err := newCLIEngine("codex").Run(t.Context(), req, wire.Discard); err == nil || !strings.Contains(err.Error(), "spy") {
		t.Fatalf("Run err = %v, want the spy's Exec error", err)
	}
	return cleaned, rendered, seen
}

func (r renderSpy) Exec(_ context.Context, p CLIPrep, _ RunOptions, _ wire.Sink) (wire.Result, error) {
	*r.seen = p
	return p.Result, errors.New("spy: no CLI")
}

// A headless run renders the request's settings after After and runs the cleanup once, even when Exec fails.
func TestCLIRunRendersSettingsAndCleansUp(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.Use(t)
	cleaned, _, seen := runSpy(t, RunOptions{Prompt: "p", Provider: tg.Provider.Name, Model: tg.Model, Effort: "high"})
	if cleaned != 1 || !slices.Equal(seen.Emulated, []string{"effort", "after-ran"}) || !slices.Contains(seen.Argv, "high") {
		t.Errorf("cleanup ran %d times, Exec saw emulated %v argv %v; want 1, [effort after-ran], --effort high",
			cleaned, seen.Emulated, seen.Argv)
	}
}

// One headless dispatch renders the settings exactly once: a second render repeats credential lookups and overlays.
func TestCLIRunRendersSettingsOnce(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.Use(t)
	if _, rendered, _ := runSpy(t, RunOptions{Prompt: "p", Provider: tg.Provider.Name, Model: tg.Model}); rendered != 1 {
		t.Errorf("RenderSettings ran %d times for one dispatch, want 1", rendered)
	}
}

// An invalid stored record fails a headless dispatch by agent and field, never value; interactive keeps the built-ins.
func TestInvalidRecordRefused(t *testing.T) {
	tg := target(t, "openai", "gpt-5")
	vaulttest.Use(t)
	if err := vault.SetAgentRecord("copilot", vault.AgentRecord{Launch: &vault.AgentLaunch{
		Env: []string{"FOO=1", "OPENAI_API_KEY=leakvalue"}, Args: []string{"--banner"}}}); err != nil {
		t.Fatal(err)
	}
	err := cliEngine{fixtureDriver{NewBaseEngine("copilot")}}.renderSettings(t.Context(), &CLIPrep{}, RunOptions{}, tg)
	if err == nil || !strings.Contains(err.Error(), "copilot launch.env") || strings.Contains(err.Error(), "leakvalue") {
		t.Errorf("headless render err = %v, want the copilot launch.env refusal without the value", err)
	}
	env, argv := SchemaFor("copilot").LaunchSpec()
	if slices.Contains(env, "FOO=1") || !slices.Equal(argv, []string{"--allow-all"}) {
		t.Errorf("interactive launch: env %v argv %v; want nothing of the record and the built-in [--allow-all]", env, argv)
	}
}

// The built-in records, alone and under a legacy overlay of the old agents.json shape, pass validation.
func TestBuiltinRecordsValidate(t *testing.T) {
	vaulttest.Use(t)
	legacy := `{"agents":{"codex":{"permission":{"approvalPolicy":"never","sandboxMode":"danger-full-access"}},
		"copilot":{"permission":{"defaultPermissionMode":"allow-all"},"launch":{"args":["--allow-all"]}},
		"gemini":{"permission":{"useGuardRules":true},"launch":{"args":["--dangerously-skip-permissions"]}},
		"opencode":{"launch":{}}}}`
	for _, overlay := range []string{"", legacy} {
		if overlay != "" {
			vaulttest.Put(t, vault.Overlay, "agents", overlay)
		}
		for _, name := range agentid.All() {
			rec, err := agentconfig.Settings(name)
			if err == nil {
				err = ValidateRecord(name, rec)
			}
			if err != nil {
				t.Errorf("legacy overlay %v: %s: %v", overlay != "", name, err)
			}
		}
	}
}

// A bad legacy launch entry still reports the stored-record error and keeps the fallback env for every agent.
func TestResolveSettingsInvalidLegacyLaunchKeepsFallback(t *testing.T) {
	for _, name := range agentid.All() {
		t.Run(name, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "agents", fmt.Sprintf(
				`{"agents":{%q:{"launch":{"env":["COPILOT_PROVIDER_BASE_URL=https://invalid.example","KEEP_ME=1"]}}}}`, name))
			st, err := ResolveSettings(name, RunOptions{}, providers.Target{}, false)
			if err == nil || !strings.Contains(err.Error(), name+" launch.env:") || st.Agent != name {
				t.Fatalf("ResolveSettings: agent=%q err=%v; want fallback and launch.env error", st.Agent, err)
			}
			var notice bytes.Buffer
			old := NoticeOut
			NoticeOut = &notice
			t.Cleanup(func() { NoticeOut = old })
			st = SchemaFor(name).launchSettings(providers.Target{}, false)
			want := fmt.Sprintf("claude-code: %s stored agent settings not applied: %v; built-in defaults used\n", name, err)
			if notice.String() != want {
				t.Fatalf("notice = %q, want %q", notice.String(), want)
			}
			route := "COPILOT_PROVIDER_BASE_URL=https://route.example"
			env, _, _, cleanup, err := NewBaseEngine(name).RenderSettings(t.Context(), st, []string{route}, nil)
			defer cleanup()
			if err != nil || !slices.Contains(env, "KEEP_ME=1") || !slices.Contains(env, route) || len(env) != 2 {
				t.Fatalf("fallback env=%v err=%v; want KEEP_ME and only the routed endpoint", env, err)
			}
		})
	}
}
