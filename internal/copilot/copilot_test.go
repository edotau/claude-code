package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestResumeSlot(t *testing.T) {
	for in, want := range map[string]string{"sonnet": "sonnet", "dev:haiku": "haiku", "claude-sonnet-5-5": "sonnet", "gpt-5": ""} {
		if got := resumeAlias(in); got != want {
			t.Errorf("resumeAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

type recSink struct{ types []string }

func (s *recSink) Emit(ev wire.Event) error { s.types = append(s.types, ev.Type); return nil }

// An approved tool then a 503 leaves the sink with a progress mark before any text; a denied-only run emits none.
func TestToolApprovalMarksProgressOnFailure(t *testing.T) {
	run := func(o sdkOptions, req copilot.PermissionRequest) []string {
		var ok atomic.Bool
		h := permissionHandler(o, &ok)
		if _, err := h(req, copilot.PermissionInvocation{}); err != nil {
			t.Fatal(err)
		}
		sink := &recSink{}
		out := sdkResult{Text: "part", ToolRan: ok.Load()}
		err := markToolRan(out, sink, errors.New("503 service unavailable")) // Run marks the tool before branching on err
		_, _ = copilotPartial(wire.Result{}, out, err, sink)
		return sink.types
	}
	if got := run(sdkOptions{AllowAllTools: true}, rpc.PermissionRequestRead{}); !slices.Equal(got, []string{wire.EventProgress, wire.EventText}) {
		t.Errorf("approved read: events = %v, want [progress text]", got)
	}
	if got := run(sdkOptions{AllowAllTools: true, ReadOnlyTools: true}, rpc.PermissionRequestMCP{}); !slices.Equal(got, []string{wire.EventText}) {
		t.Errorf("denied mcp: events = %v, want [text]", got)
	}
}

// A tool-only run (no text, no error) still marks progress, so a fallback cannot repeat its side effects.
func TestMarkToolRanMarksProgressOnSuccess(t *testing.T) {
	sink := &recSink{}
	if err := markToolRan(sdkResult{ToolRan: true}, sink, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.types) != 1 || sink.types[0] != wire.EventProgress {
		t.Fatalf("sink saw %v, want one progress mark and no tool_use", sink.types)
	}
	if err := markToolRan(sdkResult{}, sink, nil); err != nil || len(sink.types) != 1 {
		t.Fatalf("no tool ran: err=%v events=%v, want none emitted", err, sink.types)
	}
}

// A _TOKEN credential and the SDK token never land in the quota hand-off file, in prompt, partial text or session.
func TestHandoffNeverWritesCredentials(t *testing.T) {
	const sdkToken, envSecret = "gho_SDKTOKEN12345", "dbx_pat_ENVSECRET9"
	dir := t.TempDir()
	orig := handoffDir
	handoffDir = func(string) string { return dir }
	t.Cleanup(func() { handoffDir = orig })
	env := []string{providers.CopilotTokenEnv + "=" + sdkToken, "DBX_TOKEN=" + envSecret, "PLAIN=visible"}
	creds := credentialValues(env)
	req := agent.RunOptions{WorkDir: t.TempDir(), Prompt: "use " + sdkToken + " and " + envSecret}
	path, err := writeCopilotHandoff(req, wire.Result{SessionID: "sess-1"}, "partial "+envSecret,
		"acct", errors.New("quota exhausted"), creds)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(onDisk)
	if strings.Contains(s, sdkToken) || strings.Contains(s, envSecret) {
		t.Errorf("handoff holds a raw credential:\n%s", s)
	}
	if !strings.Contains(s, "REDACTED") {
		t.Errorf("handoff has no redaction marker:\n%s", s)
	}
}

// A failed run keeps the usage the collector summed before the failure.
func TestCopilotPartialKeepsUsage(t *testing.T) {
	want := wire.Usage{InputTokens: 7, OutputTokens: 3}
	res, _ := copilotPartial(wire.Result{}, sdkResult{Text: "partial", Usage: want}, errors.New("boom"), wire.Discard)
	if res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}
}

const runGHToken = "plainRunToken123456" // no token shape, so only Run's credential list can redact it

// runScratch routes copilot to gh user "work" (fake gh/copilot, free credits), stubs the SDK; returns the hand-off dir.
func runScratch(t *testing.T, ask func(context.Context, sdkOptions, string, []string) (sdkResult, error)) string {
	t.Helper()
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
	gh := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", gh)
	hosts := "github.com:\n    users:\n        work:\n            oauth_token: x\n    user: work\n"
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte("#!/bin/sh\necho "+runGHToken+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","login":"work","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"}}}}}`)
	pathstest.FakeVendor(t, "copilot", "exit 0")
	origPct, origAsk, origDir := CopilotUsedPct, copilotAsk, handoffDir
	dir := t.TempDir()
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	copilotAsk = ask
	handoffDir = func(string) string { return dir }
	t.Cleanup(func() { CopilotUsedPct, copilotAsk, handoffDir = origPct, origAsk, origDir })
	return dir
}

func runPersonal(t *testing.T) (wire.Result, error) {
	t.Helper()
	req := agent.RunOptions{Prompt: "do it", Provider: "personal", WorkDir: t.TempDir()}
	return copilotAgent.Run(context.Background(), req, wire.Discard)
}

// A quota failure on the GitHub route writes a redacted hand-off (the SDK token included) and returns a quota error.
func TestRunQuotaWritesRedactedHandoff(t *testing.T) {
	dir := runScratch(t, func(_ context.Context, o sdkOptions, _ string, _ []string) (sdkResult, error) {
		return sdkResult{Text: "half done with " + o.GitHubToken, SessionID: "abcdef1234567"}, errors.New("402 payment required")
	})
	res, err := runPersonal(t)
	if k := retry.Classify("copilot", err, false).Kind; k != retry.KindQuota {
		t.Fatalf("err = %v (kind %v), want a quota error", err, k)
	}
	if filepath.Dir(res.Handoff) != dir || res.SessionID != "abcdef1234567" || res.Provider != "personal" {
		t.Fatalf("result = %+v, want a hand-off in %s with the run's session and provider", res, dir)
	}
	body, _ := os.ReadFile(res.Handoff) // an unreadable file fails the partial-answer check below
	if s := string(body); strings.Contains(s, runGHToken) || !strings.Contains(s, "half done with REDACTED") {
		t.Errorf("hand-off leaks the SDK token or lost the partial answer:\n%s", s)
	}
}

// A non-quota failure on the same route keeps the partial answer and writes no hand-off.
func TestRunNonQuotaFailureWritesNoHandoff(t *testing.T) {
	dir := runScratch(t, func(context.Context, sdkOptions, string, []string) (sdkResult, error) {
		return sdkResult{Text: "half", SessionID: "abcdef1234567"}, errors.New("503 service unavailable")
	})
	res, err := runPersonal(t)
	if err == nil || res.Handoff != "" || res.Answer != "half" {
		t.Fatalf("res = %+v err = %v, want the partial answer, an error and no hand-off", res, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("hand-off dir holds %d file(s), want none", len(ents))
	}
}

// Only a quota error on a copilot-billed row hands off; a BYOK row or another failure does not.
func TestQuotaHandoffEligible(t *testing.T) {
	quota, outage := errors.New("402 payment required"), errors.New("503 service unavailable")
	gh := providers.Target{Provider: &providers.Provider{Auth: providers.Auth{Login: "copilot"}}}
	byok := providers.Target{Provider: &providers.Provider{}}
	for _, c := range []struct {
		name string
		t    providers.Target
		err  error
		want bool
	}{
		{"copilot row quota", gh, quota, true},
		{"byok row quota", byok, quota, false},
		{"copilot row outage", gh, outage, false},
	} {
		if got := quotaHandoffEligible(c.t, c.err); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

// A target without a provider is a fatal error.
func TestInitialResult(t *testing.T) {
	cs := copilotSchema()
	if _, err := initialResult(cs, providers.Target{Model: "auto"}); !isFatal(err) {
		t.Errorf("no provider: err = %v, want a typed fatal error", err)
	}
}

func isFatal(err error) bool {
	re, ok := errors.AsType[*retry.Error](err)
	return ok && re.Kind == retry.KindFatal
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live config or vault.
func TestMain(m *testing.M) {
	for _, name := range []string{"codex", "gemini", "opencode"} { // the render tests read these sibling rows
		if _, ok := agent.LookupSchema(name); !ok {
			fmt.Fprintf(os.Stderr, "copilot tests: the %s roster row is not linked into this test binary\n", name)
			os.Exit(2)
		}
	}
	code := hosttest.Main(m)
	removeFakeCLI() // the fake CLI is built once per package (sdk_test.go), so it outlives every test
	os.Exit(code)
}

// The quota hand-off masks --env values and the registry token, not only the route env.
func TestRunCredentialsCoverEnvAndRegistryToken(t *testing.T) {
	opts := sdkOptions{Token: func() (string, error) { return "sess-token-123456", nil }}
	req := agent.RunOptions{Env: []string{"INTERNAL_SECRET=plain-value-12345", "EMPTY="}}
	got := runCredentials(opts, []string{"DBX_TOKEN=route-secret-1"}, req)
	for _, want := range []string{"route-secret-1", "plain-value-12345", "sess-token-123456"} {
		if !slices.Contains(got, want) {
			t.Errorf("credentials %v miss %q", got, want)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("credentials %v hold an empty value", got)
	}
}

// A plain --env value (a path) must stay out of the redaction list; secret-named ones are masked.
func TestRunCredentialsIgnoresNonSecretEnv(t *testing.T) {
	req := agent.RunOptions{Env: []string{"WORKDIR=/home/user/long/project/dir", "API_TOKEN=s3cr3t-value-123"}}
	got := runCredentials(sdkOptions{}, nil, req)
	if !slices.Equal(got, []string{"s3cr3t-value-123"}) {
		t.Fatalf("runCredentials = %q; want only the secret-named value", got)
	}
}

func TestQuotaHandoffWriteFailureKeepsPartialAnswer(t *testing.T) {
	hosttest.PinScratch(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	regressionWrite(t, blocked, "blocked")
	old := handoffDir
	handoffDir = func(string) string { return filepath.Join(blocked, "handoffs") }
	t.Cleanup(func() { handoffDir = old })
	cause := errors.New("402 payment required")
	res, err := copilotQuotaHandoff(agent.RunOptions{WorkDir: t.TempDir()}, wire.Result{}, "partial-session", "partial answer", cause, nil)
	if res.Answer != "partial answer" || res.SessionID != "partial-session" || res.Handoff != "" {
		t.Fatalf("result = %+v; want partial answer and session without handoff", res)
	}
	if !errors.Is(err, cause) || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "handoff not written") {
		t.Fatalf("err = %v; want quota and handoff write errors", err)
	}
}

func regressionWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const testRouterBase = "http://127.0.0.1:9999"

func TestRoutedStrips1MSuffix(t *testing.T) {
	agenttest.StubProxy(t, testRouterBase, "rtr-secret-xyz")
	env, _, err := agent.SchemaFor("copilot").Configure(context.Background(), agenttest.LoadTarget(t, "anthropic", "claude-opus-5[1m]"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "COPILOT_MODEL=claude-opus-5") {
		t.Errorf("COPILOT_MODEL not claude-opus-5 in %q", env)
	}
}

func TestLaunchEnvCannotOverrideRouterBase(t *testing.T) {
	agenttest.StubProxy(t, testRouterBase, "rtr-secret-xyz")
	tg := agenttest.LoadTarget(t, "openai", "gpt-5")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"copilot":{"launch":{"env":["COPILOT_PROVIDER_BASE_URL=https://evil.example.com","KEEP_ME=1"]}}}}`)
	env, _, err := agent.SchemaFor("copilot").Configure(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL=") {
			n++
			if !strings.HasPrefix(kv, "COPILOT_PROVIDER_BASE_URL="+testRouterBase) {
				t.Errorf("base overridden: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("base var appears %d times: %q", n, env)
	}
	if !slices.Contains(env, "KEEP_ME=1") {
		t.Errorf("unrelated launch.env entry dropped: %q", env)
	}
}

// settingsCases are copilot's stored-record × flag cases; the SDK and interactive tests share them.
var settingsCases = []struct {
	name, flag string
	rec        *vault.AgentRecord
	effort     string
	context    string
}{
	{"no record", "", nil, "", ""},
	{"record effort", "", &vault.AgentRecord{Effort: "high", Context: "long_context"}, "high", "long_context"},
	{"flag over record", "low", &vault.AgentRecord{Effort: "high", Context: "long_context"}, "low", "long_context"},
	{"extras mode", "", &vault.AgentRecord{Extras: map[string]string{extraDefaultPermissionMode: "plan"}}, "", ""},
}

// copilotSettings stores rec (nil = none) in a hermetic vault and resolves copilot's Settings for --effort flag.
func copilotSettings(t *testing.T, rec *vault.AgentRecord, flag string, headless bool) agent.Settings {
	t.Helper()
	vaulttest.UseFreshKey(t)
	if rec != nil {
		if err := vault.SetAgentRecord("copilot", *rec); err != nil {
			t.Fatal(err)
		}
	}
	s, err := agent.ResolveSettings("copilot", agent.RunOptions{Effort: flag}, providers.Target{}, headless)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A run's SDK options take --effort over the stored effort and the stored context tier.
func TestRunSDKOptionsReadSettings(t *testing.T) {
	for _, c := range settingsCases {
		t.Run(c.name, func(t *testing.T) {
			var got sdkOptions
			runScratch(t, func(_ context.Context, o sdkOptions, _ string, _ []string) (sdkResult, error) {
				got = o
				return sdkResult{Text: "ok"}, nil
			})
			if c.rec != nil {
				if err := vault.SetAgentRecord("copilot", *c.rec); err != nil {
					t.Fatal(err)
				}
			}
			req := agent.RunOptions{Prompt: "do it", Provider: "personal", WorkDir: t.TempDir(), Effort: c.flag}
			if _, err := copilotAgent.Run(t.Context(), req, wire.Discard); err != nil {
				t.Fatal(err)
			}
			if got.ReasoningEffort != c.effort || got.Context != c.context {
				t.Errorf("ReasoningEffort, Context = %q, %q; want %q, %q", got.ReasoningEffort, got.Context, c.effort, c.context)
			}
		})
	}
}

// A headless run fails before any session on an unreadable or invalid stored record, naming the field, never a value.
func TestRunBadRecordFailsHeadless(t *testing.T) {
	for name, put := range map[string]func(*testing.T){
		"unreadable": func(t *testing.T) { vaulttest.Put(t, vault.Overlay, "agents", "{not json") },
		"credential launch.env": func(t *testing.T) {
			rec := vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"GITHUB_TOKEN=ghp-record-secret"}}}
			if err := vault.SetAgentRecord("copilot", rec); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			asked := false
			runScratch(t, func(context.Context, sdkOptions, string, []string) (sdkResult, error) {
				asked = true
				return sdkResult{Text: "ok"}, nil
			})
			put(t)
			req := agent.RunOptions{Prompt: "do it", Provider: "personal", WorkDir: t.TempDir(), Effort: "low"}
			_, err := copilotAgent.Run(t.Context(), req, wire.Discard)
			if err == nil || asked || !strings.Contains(err.Error(), "copilot") || strings.Contains(err.Error(), "ghp-record-secret") {
				t.Errorf("err = %v, session started = %v; want a copilot error before any session, no value", err, asked)
			}
			if err != nil && name == "credential launch.env" && !strings.Contains(err.Error(), "launch.env") {
				t.Errorf("err = %v, want it to name launch.env", err)
			}
		})
	}
}

// Armed on macOS a Databricks hop runs routed BYOK: router base /p/dbx, no GitHub login, the login env stripped.
func TestArmCopilotBYOK(t *testing.T) {
	got, child := runArmedBYOK(t)
	if got.UseLoggedInUser || got.GitHubToken != "" || got.multiProvider() {
		t.Errorf("UseLoggedInUser=%v GitHubToken set=%v registry=%v; want BYOK with no GitHub login",
			got.UseLoggedInUser, got.GitHubToken != "", got.multiProvider())
	}
	base := proc.EnvCopilotProviderBaseURL + "=" + testRouterBase + "/p/dbx"
	if !slices.Contains(child, base) || !slices.Contains(child, "COPILOT_OFFLINE=true") {
		t.Errorf("child env lacks %s or COPILOT_OFFLINE=true", base)
	}
	if keyed, leaked := byokKeyState(child); !keyed || leaked {
		t.Errorf("child env: BYOK key present=%v, a GitHub token or the workspace token leaked=%v", keyed, leaked)
	}
}

// An arm carried only by the run's values still puts a non-macOS route-less Databricks hop on routed BYOK.
func TestArmedBYOKFromRunValues(t *testing.T) {
	setGOOS(t, "linux")
	t.Setenv(agentconfig.OverlayVar, "")
	tg := providers.Target{Provider: &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks}}
	arm := proc.Values{agentconfig.OverlayVar: `{"provider":"databricks","profile":"dbx"}`}
	for name, c := range map[string]struct {
		values proc.Values
		want   bool
	}{"armed": {arm, true}, "unarmed": {proc.Values{}, false}} {
		if got := armedBYOK(proc.WithValues(t.Context(), c.values), nil, tg); got != c.want {
			t.Errorf("%s: armedBYOK = %v, want %v", name, got, c.want)
		}
	}
}

// byokKeyState: child carries a BYOK key (the router's credential for dbx; task 2.1 mints cct_ARM there), and whether
// a planted GitHub token or the workspace token reached it.
func byokKeyState(child []string) (keyed, leaked bool) {
	for _, kv := range child {
		k, v, _ := strings.Cut(kv, "=")
		keyed = keyed || (v != "" && (k == proc.EnvCopilotProviderBearerToken || k == proc.EnvCopilotProviderAPIKey))
		leaked = leaked || v == "ghp-planted" || (strings.HasPrefix(k, proc.CopilotProviderPrefix) && v == "dapi-fixture")
	}
	return keyed, leaked
}

// runArmedBYOK runs copilot on dbx inside databricks:dbx on macOS with GitHub tokens planted: SDK options, child env.
func runArmedBYOK(t *testing.T) (sdkOptions, []string) {
	t.Helper()
	macFallbackScratch(t)
	agenttest.StubProxy(t, testRouterBase, "rtr-secret-xyz")
	agent.MintSession = func(context.Context, []string, int) (string, error) { return "cct_ARM", nil }
	pathstest.FakeVendor(t, "copilot", "exit 0")
	t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"dbx"}`)
	t.Setenv("GH_TOKEN", "ghp-planted")
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghp-planted")
	var got sdkOptions
	orig := copilotAsk
	copilotAsk = func(_ context.Context, o sdkOptions, _ string, _ []string) (sdkResult, error) {
		got = o
		return sdkResult{Text: "ok"}, nil
	}
	t.Cleanup(func() { copilotAsk = orig })
	req := agent.RunOptions{Prompt: "hi", Provider: "dbx", Model: "databricks-claude-sonnet-5-5", WorkDir: t.TempDir()}
	if _, err := copilotAgent.Run(t.Context(), req, wire.Discard); err != nil {
		t.Fatal(err)
	}
	co, err := buildClientOptions(t.Context(), got)
	if err != nil {
		t.Fatal(err)
	}
	return got, co.Connection.(copilot.StdioConnection).Env
}

func assertCodexManagedBlock(t *testing.T, codexCfg []byte) {
	t.Helper()
	if !strings.Contains(string(codexCfg), "[model_providers.claude-code]") {
		t.Errorf("codex config missing managed block:\n%s", codexCfg)
	}
	if !strings.Contains(string(codexCfg), "# >>> claude-code:managed:begin >>>") {
		t.Errorf("codex config missing banner markers:\n%s", codexCfg)
	}
	// Top-level approval/sandbox keys from agentconfig must precede any [table] header.
	approvalIdx := strings.Index(string(codexCfg), "approval_policy")
	sandboxIdx := strings.Index(string(codexCfg), "sandbox_mode")
	firstTable := strings.Index(string(codexCfg), "[")
	if approvalIdx < 0 || sandboxIdx < 0 {
		t.Fatalf("codex config missing approval_policy/sandbox_mode:\n%s", codexCfg)
	}
	if firstTable >= 0 && (approvalIdx > firstTable || sandboxIdx > firstTable) {
		t.Errorf("approval_policy/sandbox_mode must precede the first [table] header:\n%s", codexCfg)
	}
	if n := strings.Count(string(codexCfg), `cli_auth_credentials_store = "keyring"`); n != 1 {
		t.Errorf("cli_auth_credentials_store occurs %d times, want 1:\n%s", n, codexCfg)
	}
}

// opencode's overlay is a tracked, owner-authored template, not a per-provider render: just the pinned model/variant.
func assertOpencodeOverlayOnly(t *testing.T, home string) {
	t.Helper()
	ocRaw, err := os.ReadFile(filepath.Join(paths.StateDir(), "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode overlay: %v", err)
	}
	var oc map[string]any
	if err := json.Unmarshal(ocRaw, &oc); err != nil {
		t.Fatalf("opencode.json not valid JSON: %v", err)
	}
	if _, ok := oc["model"]; !ok {
		t.Errorf("opencode.json missing a model key: %s", ocRaw)
	}
	agenttest.AssertNotExist(t, "opencode's own ~/.config/opencode/opencode.json must not be written",
		filepath.Join(home, ".config", "opencode", "opencode.json"))
}

func TestRunWritesBannerBlocksAndIsIdempotent(t *testing.T) {
	home := agenttest.Scratch(t)
	var out bytes.Buffer
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}}
	drift, err := agent.RenderConfigs(opts)
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	if !drift {
		t.Fatal("expected drift=true on first write")
	}

	codexCfg, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	assertCodexManagedBlock(t, codexCfg)
	assertOpencodeOverlayOnly(t, home)

	// gemini renders no auth setting (agy has none) and no ~/.gemini/settings.json; agy's own files live elsewhere.
	agenttest.AssertNotExist(t, "gemini must write no settings.json", filepath.Join(home, ".gemini", "settings.json"))
	if !strings.Contains(out.String(), "gemini: no auth setting to render") {
		t.Errorf("expected a gemini report-only line, got:\n%s", out.String())
	}

	// A second run with nothing changed must report no drift and touch no file's content.
	var out2 bytes.Buffer
	opts.Out = &out2
	drift, err = agent.RenderConfigs(opts)
	if err != nil {
		t.Fatalf("second agent.RenderConfigs: %v", err)
	}
	if drift {
		t.Fatalf("expected no drift on an idempotent second run, output:\n%s", out2.String())
	}
}

// TestScratchConfigDirSkipsVendorWritesWithoutFlag reproduces the P0: CLAUDE_CONFIG_DIR pointed at a
// worktree while vendor destinations stay $HOME-rooted (agentconfig.RenderOptions.Home left unset, matching production,
// where os.UserHomeDir() resolves the real process HOME). Without --vendors nothing is written under
// that HOME's vendor dirs; with it, writes land exactly as before.
func TestScratchConfigDirSkipsVendorWritesWithoutFlag(t *testing.T) {
	agenttest.Scratch(t) // sets CLAUDE_CONFIG_DIR + a keyed provider overlay; its own returned Home is unused here
	vendorHome := t.TempDir()
	t.Setenv("HOME", vendorHome)
	t.Setenv("CODEX_HOME", filepath.Join(vendorHome, ".codex"))

	var out bytes.Buffer
	drift, err := agent.RenderConfigs(agentconfig.RenderOptions{Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if drift {
		t.Fatal("a skipped render must report no drift")
	}
	if !strings.Contains(out.String(), "skipping agent configs") {
		t.Fatalf("expected the skip note, got:\n%s", out.String())
	}
	agenttest.AssertNotExist(t, "expected no vendor writes under the scratch HOME",
		filepath.Join(vendorHome, ".codex"), filepath.Join(vendorHome, ".config", "opencode"),
		filepath.Join(paths.StateDir(), "opencode"), filepath.Join(vendorHome, ".copilot"))

	var out2 bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Out: &out2, ForceHomes: true, Agents: []string{"codex", "opencode", "copilot"}}); err != nil {
		t.Fatalf("Run with --vendors: %v\n%s", err, out2.String())
	}
	agenttest.AssertExists(t, "--vendors must still write the vendor configs",
		filepath.Join(vendorHome, ".codex", "config.toml"), filepath.Join(paths.StateDir(), "opencode", "opencode.json"),
		filepath.Join(vendorHome, ".copilot", "hooks", "claude-code-harness.json"))
}

const parityRows = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}}},` +
	`"anthropic":{"alt":{"base_url":"https://alt.example.com","protocol":"anthropic","auth":{"type":"none"},"models":{"opus":"claude-opus-5-5"}}}}}`

var codexModelArg = regexp.MustCompile(`^model="(.*)"$`)

func launchModel(t *testing.T, name string, o agent.LaunchOptions) string {
	t.Helper()
	o.PrintEnv = true
	plan, err := agent.LaunchPlan(context.Background(), name, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := plan.Set["COPILOT_MODEL"]; m != "" {
		return m
	}
	for i, a := range plan.Argv {
		if m := codexModelArg.FindStringSubmatch(a); m != nil && i > 0 && plan.Argv[i-1] == "-c" {
			return m[1]
		}
		if a == "--model" && i+1 < len(plan.Argv) {
			return plan.Argv[i+1]
		}
	}
	t.Fatalf("no model in %v", plan.Argv)
	return ""
}

func TestLaunchAndAgentAgreeOnModel(t *testing.T) {
	cases := []struct{ agent, profile, flag string }{
		{"codex", "dev", ""},
		{"codex", "dev", "gpt-5.5"},
		{"copilot", "alt", ""},
		{"copilot", "alt", "claude-sonnet-5-5"},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.profile+"/"+c.flag, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", parityRows)
			pathstest.FakeVendor(t, c.agent, "")
			pin := "databricks-gpt-6-1-sol"
			if c.agent == "copilot" {
				pin = "claude-haiku-5-5"
			}
			vaulttest.SetRecord(t, c.agent, "model", pin)
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			tg, _, err := agent.SchemaFor(c.agent).Target(agent.RunOptions{Provider: c.profile, Model: c.flag})
			if err != nil {
				t.Fatal(err)
			}
			got := launchModel(t, c.agent, agent.LaunchOptions{Provider: c.profile, Model: c.flag})
			if got != tg.Model {
				t.Errorf("launch %q vs agent %q", got, tg.Model)
			}
			if c.flag == "" && !strings.Contains(got, pin) {
				t.Errorf("model %q ignores the pin %q", got, pin)
			}
		})
	}
}

const agentHdr = "X-Claude-Code-Agent"

// withHeaders returns tg with the provider's own headers set.
func withHeaders(tg providers.Target, h map[string]string) providers.Target {
	p := *tg.Provider
	p.Headers = h
	tg.Provider = &p
	return tg
}

// A direct dispatch never passes routerRoute, so it carries the provider's own headers and no agent tag.
func TestDirectDispatchesHaveNoAgentTag(t *testing.T) {
	vaulttest.Use(t)
	ctx := agent.WithDirect(context.Background())
	own := map[string]string{"X-Tenant": "t1"}

	_, argv, err := agent.SchemaFor("codex").Configure(ctx, withHeaders(agenttest.LoadTarget(t, "openai", "gpt-5"), own))
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(argv, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
		t.Errorf("direct codex headers wrong: %q", argv)
	}
	for _, v := range []string{"opencode", "copilot"} {
		env, _, err := agent.SchemaFor(v).Configure(ctx, withHeaders(agenttest.LoadTarget(t, "openai", "gpt-5"), own))
		if err != nil {
			t.Fatal(err)
		}
		if j := strings.Join(env, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
			t.Errorf("direct %s headers wrong: %q", v, env)
		}
	}
}

// TestLookupKnownVendors: every external vendor is a registered, external schema with an engine of its own;
// resume, catalog and read-permission facts are pinned by TestRosterFacts.
func TestLookupKnownVendors(t *testing.T) {
	var engines []string
	for _, e := range agent.Engines() {
		engines = append(engines, e.Name())
	}
	for _, name := range []string{"codex", "gemini", "copilot", "opencode"} {
		s, ok := agent.LookupSchema(name)
		if !ok {
			t.Errorf("LookupSchema(%q) not found", name)
			continue
		}
		if s.Name != name || !s.External() {
			t.Errorf("LookupSchema(%q) = %q, external %v", name, s.Name, s.External())
		}
		if !slices.Contains(engines, name) {
			t.Errorf("schema %q has no engine", name)
		}
	}
}

// TestRosterPromptTransport: every uncapped row names its transport (the zero value is the capped argv),
// and gemini's verified ceiling sits below its stdin transport.
func TestRosterPromptTransport(t *testing.T) {
	want := map[string]agent.PromptTransport{"claude": agent.PromptStdin, "codex": agent.PromptStdin,
		"copilot": agent.PromptMessage, "opencode": agent.PromptArgv, "gemini": agent.PromptStdin}
	for name, via := range want {
		s, ok := agent.LookupSchema(name)
		if !ok || s.CLI.PromptVia != via {
			t.Errorf("%s PromptVia = %v (found %v), want %v", name, s.CLI.PromptVia, ok, via)
		}
	}
	if agent.PromptArgv.MaxBytes() != agent.MaxPromptBytes || agent.PromptStdin.MaxBytes() != 0 {
		t.Errorf("MaxBytes: argv %d stdin %d", agent.PromptArgv.MaxBytes(), agent.PromptStdin.MaxBytes())
	}
	if g, _ := agent.LookupSchema("gemini"); g.CLI.PromptMaxBytes != 100*1024 {
		t.Errorf("gemini PromptMaxBytes = %d, want %d", g.CLI.PromptMaxBytes, 100*1024)
	}
}

func TestRosterMatchesSpecs(t *testing.T) {
	var names, runners []string
	for _, s := range agent.Roster() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"claude", "codex", "copilot", "gemini", "opencode"}) {
		t.Fatalf("roster order = %v", names)
	}
	for _, r := range agent.Engines() {
		runners = append(runners, r.Name())
	}
	for label, other := range map[string][]string{
		"Engines": runners, "RouteAgents": slices.Clone(providers.RouteAgents),
	} {
		slices.Sort(other)
		want := slices.Clone(names)
		slices.Sort(want)
		if !slices.Equal(other, want) {
			t.Errorf("%s names = %v, roster names = %v", label, other, want)
		}
	}
}

func TestRosterFacts(t *testing.T) {
	catalogs := map[string]agent.CLI{
		"codex":    {ResumeFormat: "codex resume session", ModelsProvider: "openai"},
		"gemini":   {ResumeFormat: "agy --conversation session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"opencode": {ResumeFormat: "opencode run --session session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"copilot":  {ResumeFormat: "copilot --resume session", ModelsProvider: "github", ReadNeedsTools: true},
	}
	for _, s := range agent.Roster() {
		t.Run(s.Name, func(t *testing.T) {
			got, ok := agent.LookupSchema(s.Name)
			if !ok || !reflect.DeepEqual(got, s) {
				t.Fatalf("LookupSchema(%q) = %+v, %v; want %+v", s.Name, got, ok, s)
			}
			if s.External() != (s.Name != "claude") {
				t.Errorf("External() = %v", s.External())
			}
			assertRosterWireFacts(t, s, catalogs[s.Name])
		})
	}
	if s, ok := agent.LookupSchema("unknown"); ok || !reflect.DeepEqual(s, agent.Schema{}) {
		t.Errorf("unknown schema = %+v, %v", s, ok)
	}
	if (agent.Schema{}).External() {
		t.Error("zero schema is external")
	}
}

func assertRosterWireFacts(t *testing.T, s agent.Schema, want agent.CLI) {
	t.Helper()
	if s.Name == "claude" {
		if s.CLI.Binary != "" || s.CLI.Dialects != nil || s.CLI.RouterSuffix != nil {
			t.Errorf("claude wire facts = %+v", s.CLI)
		}
		return
	}
	if s.ResumeHint("session") != want.ResumeFormat {
		t.Error("resume command changed")
	}
	if !slices.Equal(s.CLI.ModelsArgv, want.ModelsArgv) || s.CLI.ModelsProvider != want.ModelsProvider ||
		s.CLI.ReadNeedsTools != want.ReadNeedsTools || !slices.Equal(s.CLI.ExtraUsage, want.ExtraUsage) {
		t.Error("catalog, tools or help facts changed")
	}
}
