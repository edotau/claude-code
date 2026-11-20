package agent

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeRunner fails with errs[attempt] and records the request it saw.
type fakeRunner struct {
	name        string // defaults to "fake"
	caps        Caps
	errs        []error
	write       string
	emptyAnswer bool // forces Result.Answer to "" instead of the default "a"
	seen        []RunOptions
}

func (f *fakeRunner) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}
func (f *fakeRunner) Caps() Caps       { return f.caps }
func (f *fakeRunner) Available() error { return nil }
func (f *fakeRunner) Run(_ context.Context, req RunOptions, w wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	w.Emit(wire.Event{Type: wire.EventText, Text: f.write})
	err := f.errs[len(f.seen)-1]
	ans := "a"
	if f.emptyAnswer {
		ans = ""
	}
	return wire.Result{Answer: ans}, err
}

// toolRunner reports a tool call, no text, then a transient failure: the tool may have had effects.
type toolRunner struct{ fakeRunner }

func (f *toolRunner) Run(_ context.Context, req RunOptions, w wire.Sink) (wire.Result, error) {
	f.seen = append(f.seen, req)
	_ = w.Emit(wire.ToolEvent("Bash", "t1"))
	return wire.Result{}, io.ErrUnexpectedEOF
}

// codexHang blocks until its context is done, named "codex" so RunWithFallback's class check applies.
type codexHang struct {
	stubEngine
	calls int
}

func newCodexHang() *codexHang {
	return &codexHang{name: "codex", caps: Caps{Tools: Native}}
}

func (h *codexHang) Run(ctx context.Context, _ RunOptions, _ wire.Sink) (wire.Result, error) {
	h.calls++
	<-ctx.Done()
	return wire.Result{}, ctx.Err()
}

// A flag the agent lacks is dropped from the request and named in Dropped, whichever flag it is.
func TestRunDropsFlagsAgentLacks(t *testing.T) {
	setConfigDir(t)
	cases := []struct {
		name string
		req  RunOptions
		want []string
	}{
		{"effort", RunOptions{Prompt: "p", Model: "m", Effort: "max"}, []string{"--effort=max"}},
		{"fleet", RunOptions{Prompt: "p", Fleet: true}, []string{"--fleet"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{nil}}
			res, err := Run(context.Background(), f, c.req, wire.Discard, nil)
			if err != nil || res.Agent != "fake" || !reflect.DeepEqual(res.Dropped, c.want) || f.seen[0].Effort != "" || f.seen[0].Fleet {
				t.Fatalf("res=%+v err=%v seen=%+v", res, err, f.seen)
			}
		})
	}
}

// noteRunner returns a vendor note in Result.Emulated, as codex does for an effort clamp.
type noteRunner struct{ fakeRunner }

func (f *noteRunner) Run(ctx context.Context, req RunOptions, w wire.Sink) (wire.Result, error) {
	res, err := f.fakeRunner.Run(ctx, req, w)
	res.Emulated = []string{"vendor-note"}
	return res, err
}

// The envelope keeps the run's own emulated entries and the vendor's note, run-level first.
func TestRunKeepsVendorEmulatedNotes(t *testing.T) {
	setConfigDir(t)
	f := &noteRunner{fakeRunner{caps: Caps{Model: Native, SystemPrompt: Emulated}, errs: []error{nil}}}
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p", SystemPrompt: "sys"}, wire.Discard, nil)
	if want := []string{"--system-prompt", "vendor-note"}; err != nil || !reflect.DeepEqual(res.Emulated, want) {
		t.Fatalf("Emulated=%v want %v err=%v", res.Emulated, want, err)
	}
}

// Streamed text or a tool call that may have had effects vetoes the retry: one attempt, Progressed set.
func TestRunProgressVetoesRetry(t *testing.T) {
	setConfigDir(t)
	text := &fakeRunner{write: "partial", errs: []error{errors.New("HTTP 503 overloaded"), nil}}
	tool := &toolRunner{}
	cases := []struct {
		name string
		r    Engine
		seen func() int
	}{
		{"streamed output", text, func() int { return len(text.seen) }},
		{"tool call", tool, func() int { return len(tool.seen) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			_, err := Run(context.Background(), c.r, RunOptions{Prompt: "p"}, wire.NewTextSink(&out), nil)
			var e *retry.Error
			if !errors.As(err, &e) || !e.Progressed || c.seen() != 1 {
				t.Fatalf("progress must veto retry: err=%v attempts=%d", err, c.seen())
			}
		})
	}
}

// A hard-quota 429 (this key cannot use the model) demotes the alias for the retry instead of waiting it out.
func TestRunDemotesSlotOnHardQuota(t *testing.T) {
	fastDefaultBackoff(t)
	setConfigDir(t)
	hard := errors.New("HTTP 429: You exceeded your current quota. Quota exceeded for metric: requests, limit: 0, model: big")
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{hard, nil}}
	var notes []string
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, func(s string) { notes = append(notes, s) })
	if err != nil || len(f.seen) != 2 || f.seen[0].Model != "" || f.seen[1].Model != "sonnet" {
		t.Fatalf("err=%v seen=%+v res=%+v", err, f.seen, res)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "quota-dead") {
		t.Errorf("demotion must be announced: %v", notes)
	}

	// A transient 429 keeps the model: nothing to demote.
	f = &fakeRunner{caps: Caps{Model: Native}, errs: []error{errors.New("HTTP 429 rate limited, try again in 1s"), nil}}
	if _, err := Run(context.Background(), f, RunOptions{Prompt: "p", Model: "opus"}, wire.Discard, nil); err != nil || f.seen[1].Model != "opus" {
		t.Fatalf("transient 429 must retry the same alias: err=%v seen=%+v", err, f.seen)
	}
}

// A 402 (account exhausted) is KindQuota and IsQuota but not ModelDenied: a lower alias is as exhausted, so no demote.
func assertQuotaNoDemote(t *testing.T, acctErr error) {
	t.Helper()
	f := &fakeRunner{caps: Caps{Model: Native}, errs: []error{acctErr}}
	var notes []string
	_, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, func(s string) { notes = append(notes, s) })
	var e *retry.Error
	if !errors.As(err, &e) || e.Kind != retry.KindQuota {
		t.Fatalf("err = %v, want a KindQuota error", err)
	}
	if len(f.seen) != 1 || f.seen[0].Model != "" {
		t.Fatalf("seen = %+v, want one attempt with the model never demoted off \"\"", f.seen)
	}
	if len(notes) != 0 {
		t.Errorf("account exhaustion must not announce an alias demotion: %v", notes)
	}
}

func TestRunQuotaAccountExhaustedDoesNotDemote(t *testing.T) {
	fastDefaultBackoff(t)
	setConfigDir(t)
	assertQuotaNoDemote(t, errors.New("HTTP 402 payment required: you have exceeded your monthly quota"))

	// A body with BOTH the 402 account marker and a model-denied marker is exhausted: hardAccountRE vetoes ModelDenied.
	assertQuotaNoDemote(t, errors.New("402 payment required: insufficient_quota"))
}

// Do's AttemptTimeout-is-terminal path is the only timeout branch left: a hung call is KindFatal (DeadlineExceeded).
func TestRunTimeoutEndsKindFatalRegardlessOfTools(t *testing.T) {
	setConfigDir(t)
	for _, tools := range []bool{true, false} {
		t.Run(fmt.Sprintf("Tools=%v", tools), func(t *testing.T) {
			hang := newCodexHang()
			_, err := Run(context.Background(), hang, RunOptions{Prompt: "p", Tools: tools, Timeout: 20 * time.Millisecond}, wire.Discard, nil)
			var e *retry.Error
			if !errors.As(err, &e) || e.Kind != retry.KindFatal || !errors.Is(err, context.DeadlineExceeded) || hang.calls != 1 {
				t.Fatalf("Tools=%v: calls=%d err=%v, want KindFatal wrapping DeadlineExceeded after one call", tools, hang.calls, err)
			}
		})
	}
}

func TestRunStatusAndAttempts(t *testing.T) {
	setConfigDir(t)
	fastDefaultBackoff(t)
	dl := context.DeadlineExceeded
	cases := []struct {
		name     string
		write    string
		errs     []error
		status   string
		attempts int
	}{
		{"ok", "", []error{nil}, wire.StatusOK, 1},
		{"retried transient then ok", "", []error{errors.New("HTTP 503 overloaded"), nil}, wire.StatusOK, 2},
		{"quota", "", []error{&retry.Error{Kind: retry.KindQuota, Err: errors.New("plan exhausted")}}, wire.StatusQuota, 1},
		{"partial", "half", []error{errors.New("boom")}, wire.StatusPartial, 1},
		{"stalled", "", []error{dl, dl, dl}, wire.StatusStalled, 3},
		{"error", "", []error{errors.New("bad request")}, wire.StatusError, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{write: c.write, errs: c.errs}
			res, _ := Run(context.Background(), f, RunOptions{Prompt: "p"}, wire.Discard, nil)
			if res.Status != c.status || res.Transport == nil || res.Transport.Attempts != c.attempts {
				t.Fatalf("status=%q transport=%+v, want %q attempts=%d", res.Status, res.Transport, c.status, c.attempts)
			}
		})
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	StubProxyForTest("http://127.0.0.1:9999", "cct_stub-session")
	StubRefreshListingForTest() // a stale listing would otherwise claim a discover marker
	if err := registerFixtureRoster(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(hosttest.Main(m))
}

// quietStderr sends os.Stderr to /dev/null for the test; production notices read os.Stderr at call time.
func quietStderr(t *testing.T) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = null
	t.Cleanup(func() { os.Stderr = old; null.Close() })
}

// activateAgents turns the external agents on (an external agent with no profile is off) via registry profiles.
func activateAgents(t *testing.T, agents ...string) {
	t.Helper()
	profiles := map[string]string{"codex": "chatgpt", "gemini": "antigravity", "copilot": "personal", "opencode": "nvidia"}
	if err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		for _, a := range agents {
			r := m[a]
			r.Profile = profiles[a]
			m[a] = r
		}
	}); err != nil {
		t.Fatal(err)
	}
}

func allConfigured(string, router.Cell) error { return nil }

var now0 = time.Date(2026, 10, 25, 2, 6, 0, 0, time.Local)

const secret = "sk-SECRET-123"

// fakeJWT is an unsigned token whose iss claim is https://<iss>.
func fakeJWT(iss string) string {
	return hosttest.UnsignedJWT(`{"iss":"https://` + iss + `"}`)
}

// stubEngine is the Name/Caps/Available base for runner stubs; embed it and add Run.
type stubEngine struct {
	name string // defaults to "fake"
	caps Caps
}

func (s stubEngine) Name() string {
	if s.name != "" {
		return s.name
	}
	return "fake"
}

func (s stubEngine) Caps() Caps { return s.caps }

func (s stubEngine) Available() error { return nil }

func hermetic(t *testing.T, userProviders string) {
	t.Helper()
	hosttest.HermeticEnv(t)
	if userProviders != "" {
		vaulttest.Put(t, vault.Overlay, "providers", userProviders)
	}
	t.Cleanup(StubRouter("http://127.0.0.1:4000"))
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }

func target(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir()) // isolates GeminiLoginPresent's AgyLoginMarker lookup from the real machine
	writeGatewayOverlay(t)
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, secret)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get(provider)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Target{Provider: p, Model: model}
}

// writeGatewayOverlay registers a test-only third-party gateway (no such provider is built in).
func writeGatewayOverlay(t *testing.T) {
	t.Helper()
	body := `{"providers":{"anthropic":{"gateway":{"protocol":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"key","header":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

// gemini read-only runs with tools on and the read-only deny rules, never refused or dropped to tools-off.
func TestCheckRunGeminiReadOnlyDenies(t *testing.T) {
	g, err := Lookup("gemini")
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkRun(g, RunOptions{Tools: true, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range permission.ReadOnlyDeny {
		if !slices.Contains(got.Permissions.Deny, r) {
			t.Errorf("deny %v lacks %s", got.Permissions.Deny, r)
		}
	}
	if out, dropped, _ := applyCaps(g.Caps(), got); !out.Tools || !out.ReadOnly || len(dropped) != 0 {
		t.Errorf("applyCaps: tools=%v read-only=%v dropped=%v, want both kept", out.Tools, out.ReadOnly, dropped)
	}
}

// A gated read-only launch gets GIT_OPTIONAL_LOCKS=0 last (after --env); a tools run does not.
func TestCheckRunReadOnlyDisablesGitOptionalLocks(t *testing.T) {
	g, err := Lookup("gemini")
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkRun(g, RunOptions{Tools: true, ReadOnly: true, Env: []string{"GIT_OPTIONAL_LOCKS=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.Env); n != 2 || got.Env[n-1] != "GIT_OPTIONAL_LOCKS=0" {
		t.Errorf("read-only env = %d; want the caller's entry then GIT_OPTIONAL_LOCKS=0 last", len(got.Env))
	}
	if got, err = checkRun(g, RunOptions{Tools: true}); err != nil || slices.Contains(got.Env, "GIT_OPTIONAL_LOCKS=0") {
		t.Errorf("tools run env = %d (err %v); want no GIT_OPTIONAL_LOCKS", len(got.Env), err)
	}
}

// ancestorPolicyCase is one root request and what a grandchild's checkRun must still show of it.
type ancestorPolicyCase struct {
	name  string
	outer RunOptions
	holds func(RunOptions) bool
}

// ancestorPolicyCases cover inherited deny, ask, read-only and plan, and an ancestor allow that must not pass down.
func ancestorPolicyCases() []ancestorPolicyCase {
	curl, edit, git := permission.Rule{Tool: "Bash", Arg: "curl *"}, permission.Rule{Tool: "Edit", Arg: "/x/**"}, permission.Rule{Tool: "Bash", Arg: "git *"}
	return []ancestorPolicyCase{
		{"deny bash", RunOptions{Tools: true, Permissions: permission.Permissions{Deny: []permission.Rule{curl}}},
			func(o RunOptions) bool { return slices.Contains(o.Permissions.Deny, curl) }},
		{"deny edit", RunOptions{Tools: true, Permissions: permission.Permissions{Deny: []permission.Rule{edit}}},
			func(o RunOptions) bool { return slices.Contains(o.Permissions.Deny, edit) }},
		{"ask", RunOptions{Tools: true, Permissions: permission.Permissions{Ask: []permission.Rule{curl}}},
			func(o RunOptions) bool { return slices.Contains(o.Permissions.Ask, curl) }},
		{"read-only", RunOptions{Tools: true, ReadOnly: true}, func(o RunOptions) bool { return o.ReadOnly }},
		{"plan", RunOptions{Permissions: permission.Permissions{Mode: "plan"}}, func(o RunOptions) bool { return o.Permissions.Mode == "plan" }},
		{"allow stays with the ancestor", RunOptions{Tools: true, Permissions: permission.Permissions{Allow: []permission.Rule{git}}},
			func(o RunOptions) bool { return !slices.Contains(o.Permissions.Allow, git) }},
	}
}

// A leaf that clears or overwrites the policy env still gets every recorded ancestor's deny, ask, plan and read-only.
func TestCheckRunKeepsRecordedAncestorPolicy(t *testing.T) {
	for _, tc := range ancestorPolicyCases() {
		setConfigDir(t)
		root := Record{ID: "root", Agent: "copilot", Status: "running", Request: tc.outer}
		mid := Record{ID: "mid", Agent: "codex", Status: "running", Parent: "root", Depth: 1}
		if err := cmp.Or(root.Save(), mid.Save()); err != nil {
			t.Fatal(err)
		}
		for _, name := range Names() {
			e, _ := Lookup(name)
			for _, policy := range []string{"", "{}"} {
				values := proc.Values{proc.DispatchIDEnv: "mid", DispatchPermissionsEnv: policy}
				got, err := checkRun(e, RunOptions{Tools: true, Values: values})
				if err != nil || !tc.holds(got) {
					t.Errorf("%s: %s child, policy env %q: %+v read-only=%v, err %v", tc.name, name, policy, got.Permissions, got.ReadOnly, err)
				}
			}
		}
	}
}

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m, f := retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor
	retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = time.Millisecond, 2*time.Millisecond, time.Millisecond
	t.Cleanup(func() { retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = b, m, f })
}

func TestRunAuthRecoveryUsesCodexPinnedProvider(t *testing.T) {
	vaulttest.Use(t)
	fastDefaultBackoff(t)
	marker := filepath.Join(t.TempDir(), "refreshed")
	installRecoveryProfiles(t, marker)
	t.Setenv(providers.EnvSessionProfile, "regression-session")
	vaulttest.SetRecord(t, "codex", "profile", "regression-pinned")
	r := &fakeRunner{name: "codex", errs: []error{errors.New("401 Unauthorized"), nil}}
	res, err := Run(context.Background(), r, RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Transport.Attempts != 2 {
		t.Fatalf("Run = %+v, %v; want auth recovery followed by a second attempt", res, err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "regression-pinned" {
		t.Fatalf("refreshed provider = %q, %v; want Codex's pin, not the session", got, err)
	}
}

func installRecoveryProfiles(t *testing.T, marker string) {
	t.Helper()
	var profiles []string
	for _, name := range []string{"regression-session", "regression-pinned"} {
		command := fmt.Sprintf("printf %s > %q; printf test-token", name, marker)
		profiles = append(profiles, fmt.Sprintf(`%q:{"protocol":"openai","base_url":"http://127.0.0.1:1","auth":{"type":"key","command":%q},"models":{"opus":"gpt-6.1-sol"}}`, name, command))
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"openai":{`+strings.Join(profiles, ",")+`}}}`)
}

func assertFullIDTargetRewrite(t *testing.T) {
	t.Helper()
	_, viaSpec, err1 := ResolveRequestTarget(RunOptions{Model: "dev:claude-opus-5-5"})
	_, viaFlags, err2 := ResolveRequestTarget(RunOptions{Provider: "dev", Model: "claude-opus-5-5"})
	if err1 != nil || err2 != nil || viaSpec.Provider == nil || viaSpec.Provider.Name != "dev" ||
		viaSpec.Model != viaFlags.Model {
		t.Errorf("spec %+v (%v) vs flags %+v (%v)", viaSpec, err1, viaFlags, err2)
	}
}

func assertUnknownTargetRewrite(t *testing.T, model string) {
	t.Helper()
	_, bad, err := ResolveRequestTarget(RunOptions{Model: "unknownrow:opus"})
	if err == nil && bad.Provider != nil && bad.Provider.Name == "unknownrow" {
		t.Errorf("unknownrow:opus rewritten: %+v", bad)
	}
	_, nt, _ := ResolveRequestTarget(RunOptions{Model: "dev:notatier"})
	if nt.Provider != nil && nt.Provider.Name == "dev" && nt.Model == model {
		t.Errorf("dev:notatier rewritten to dev's opus alias: %+v", nt)
	}
}

// The host mismatch of a prefix-routed vendor request fails as auth and the agent never starts.
func TestRunPrefixRoutedHostMismatchFailsAuth(t *testing.T) {
	hmProfile(t, "true")
	t.Setenv("HM_KEY", fakeJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"HM_KEY"},"models":{"opus":"m"}}}}}`)
	r := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{nil}}
	_, err := Run(withIsolation(context.Background()), r, RunOptions{Prompt: "p", Model: "hm:opus"}, wire.Discard, nil)
	var e *retry.Error
	if !errors.As(err, &e) || e.Kind != retry.KindAuth || len(r.seen) != 0 {
		t.Fatalf("err = %v, runs = %d; want KindAuth before any run", err, len(r.seen))
	}
}

// A transient credential-command failure is retried inside Run instead of failing the hop as auth.
func TestRunTransientCredentialFailureIsRetried(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmt%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, fakeJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	res, err := Run(context.Background(), r, RunOptions{Prompt: "p", Provider: name}, wire.Discard, nil)
	if err != nil || res.Answer != "a" || res.Transport.Attempts != 2 {
		t.Fatalf("res=%+v err=%v; want an answer on attempt 2", res, err)
	}
}

// A vault overlay's default policy reaches the engine under the request: Deny holds, request allow and mode widen.
func TestRunLayersAgentDefaultsUnderTheRequest(t *testing.T) {
	hermetic(t, "")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"fake":{"permission":{"defaults":{"mode":"plan","ask":["Bash(git push:*)"],"deny":["Bash(rm:*)"]}}}}}`)
	f := &fakeRunner{caps: Caps{Permissions: Native}, errs: []error{nil}}
	req := RunOptions{Prompt: "p", Permissions: mustPerms(t, "auto", []string{"Bash(git push:*)", "Bash(git log:*)"}, nil, nil)}
	if _, err := Run(context.Background(), f, req, nil, nil); err != nil {
		t.Fatal(err)
	}
	got := f.seen[0].Permissions
	want := `{"mode":"auto","allow":[{"Tool":"Bash","Arg":"git push:*"},{"Tool":"Bash","Arg":"git log:*"}],"deny":[{"Tool":"Bash","Arg":"rm:*"}]}`
	if permsJSON(t, got) != want {
		t.Errorf("engine saw %s\nwant %s", permsJSON(t, got), want)
	}
	if d := got.Decide("Bash", "git log && rm -rf x"); d != permission.Deny {
		t.Errorf("the default permission.Deny must hold against a compound command, got %v", d)
	}
}

// An unparseable default rule refuses the dispatch instead of running without the default policy.
func TestRunRefusesAnUnreadableDefault(t *testing.T) {
	hermetic(t, "")
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"fake":{"permission":{"defaults":{"deny":["Bash("]}}}}}`)
	f := &fakeRunner{caps: Caps{Permissions: Native}, errs: []error{nil}}
	_, err := Run(context.Background(), f, RunOptions{Prompt: "p"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "fake default permissions") || len(f.seen) != 0 {
		t.Fatalf("err %v, runs %d; want a refusal naming the default", err, len(f.seen))
	}
}

func TestRunPolicyToolsTimeout(t *testing.T) {
	for _, c := range []struct {
		req  RunOptions
		want time.Duration
	}{
		{RunOptions{Tools: true}, DefaultToolsTimeout},
		{RunOptions{}, retry.DefaultPolicy().AttemptTimeout},
		{RunOptions{Tools: true, Timeout: 5 * time.Minute}, 5 * time.Minute},
	} {
		if got := runPolicy(context.Background(), c.req, nil).AttemptTimeout; got != c.want {
			t.Errorf("runPolicy(%+v) attempt timeout = %v, want %v", c.req, got, c.want)
		}
	}
}

// Explicit Model/Provider/Effort beat the definition's.
func TestResolveOverlayAndAgentsPrecedence(t *testing.T) {
	c := &Client{}
	defs := map[string]*Schema{"sec": {Name: "sec", Prompt: "client", Model: "cm", Profile: "cp", Effort: "low"}}
	o := RunOptions{Agent: "sec", Agents: defs, Model: "mine", Provider: "myprov", Effort: "ultra"}
	if err := c.resolve(&o); err != nil || o.Model != "mine" || o.Provider != "myprov" || o.Effort != "ultra" || o.Agent != "claude" {
		t.Fatalf("explicit options lost: %+v, %v", o, err)
	}
	t.Run("definition defaults", func(t *testing.T) { assertDefinitionOverlay(t, c, defs) })
	if o = (RunOptions{Agent: "ghost"}); c.resolve(&o) == nil {
		t.Error("an unknown ref must fail resolve")
	}
}

func assertDefinitionOverlay(t *testing.T, c *Client, defs map[string]*Schema) {
	t.Helper()
	o := RunOptions{Agent: "sec", Agents: defs}
	if err := c.resolve(&o); err != nil || o.Model != "cm" || o.Provider != "cp" || o.Effort != "low" || o.Persona.Prompt != "client" {
		t.Fatalf("definition not overlaid: %+v, %v", o, err)
	}
}

// A definition with `model: inherit` carries no model of its own, so nothing reaches routing.
func TestOverlayInheritModelIsNone(t *testing.T) {
	c := &Client{}
	o := RunOptions{Agent: "plan", Agents: map[string]*Schema{"plan": {Name: "plan", Prompt: "p", Model: "inherit"}}}
	if err := c.resolve(&o); err != nil || o.Model != "" || o.Persona.Model != "inherit" {
		t.Fatalf("inherit leaked into the request model: %+v, %v", o, err)
	}
}

// Record resolves the selector to its agent but, unlike Prepare, never routes or applies the rubric.
func TestClientRecordSkipsRouting(t *testing.T) {
	setConfigDir(t)
	defs, err := ParseAgents(`{"sec":{"prompt":"p","agent":"codex"}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(nil, nil, nil)
	o := RunOptions{Agent: "sec", Agents: defs, Prompt: "x", Provider: "no-such-provider", Rubric: "/nonexistent/rubric.md"}
	if _, err := c.Prepare(t.Context(), o, nil); err == nil {
		t.Fatal("control: Prepare should fail on the unknown provider")
	}
	rec, err := c.Record(t.Context(), o, nil)
	if err != nil || rec.Agent != "codex" {
		t.Fatalf("Record = agent %q, err %v; want codex and no error", rec.Agent, err)
	}
}

// setupWorkdirFixture isolates config state and makes opencode configured without a login, so only the repo rule gates.
func setupWorkdirFixture(t *testing.T) {
	t.Helper()
	setConfigDir(t)
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	t.Setenv("OPENCODE_API_KEY", "test-key")
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := vault.SetAgentRecord("opencode", vault.AgentRecord{Profile: "zen-free"}); err != nil { // no profile = off
		t.Fatal(err)
	}
}

// Run keeps going when the dispatch record can't be saved, but says so through Notify instead of dropping it.
func TestClientRunNotifiesUnsavedRecord(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir()) // no agent binary: the dispatch itself fails fast
	state := filepath.Join(cfg, "state", "harness")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "dispatch"), nil, 0o600); err != nil { // a file where the dir goes
		t.Fatal(err)
	}
	var notes []string
	c := NewClient(nil, nil, func(s string) { notes = append(notes, s) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = c.Run(ctx, RunOptions{Agent: "claude", Provider: "ultra", Prompt: "x"}, nil)
	if !slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, "record not saved before the run") }) {
		t.Fatalf("notes = %q, want the pre-run 'record not saved' notice", notes)
	}
}

// burstRunner emits n text events, then blocks on ctx when hold is set.
type burstRunner struct {
	n    int
	hold bool
	ctx  chan context.Context
}

func (*burstRunner) Name() string     { return "fake" }
func (*burstRunner) Caps() Caps       { return Caps{} }
func (*burstRunner) Available() error { return nil }
func (b *burstRunner) Run(ctx context.Context, _ RunOptions, w wire.Sink) (wire.Result, error) {
	for range b.n {
		_ = w.Emit(wire.Event{Type: wire.EventText, Text: "x"})
	}
	if b.ctx != nil {
		b.ctx <- ctx
	}
	if b.hold {
		<-ctx.Done()
		return wire.Result{}, context.Cause(ctx)
	}
	return wire.Result{Answer: "a"}, nil
}

func streamPrepared(t *testing.T, r Engine) *Prepared {
	t.Helper()
	setConfigDir(t)
	return &Prepared{Opts: RunOptions{Agent: "fake", Prompt: "p"}, Record: ClaimRecord("fake", RunOptions{Prompt: "p"}, time.Now()), Chain: Single(r)}
}

// streamPreparedRun streams p through c.Exec, as Stream does for a RunOptions.
func streamPreparedRun(c *Client, p *Prepared) <-chan wire.Event {
	return c.streamWith(context.Background(), cmp.Or(p.Opts.Agent, "claude"), func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		return c.Exec(ctx, p, sink, RunRecordedOpts{})
	})
}

func drainEvents(ch <-chan wire.Event) (results int, last wire.Event) {
	for ev := range ch {
		if ev.Type == wire.EventResult {
			results++
		}
		last = ev
	}
	return results, last
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() <= base {
			return
		}
	}
	t.Fatalf("goroutines = %d, want <= %d: the run leaked", runtime.NumGoroutine(), base)
}

// Given a consumer that reads nothing, When the run emits 3x the buffer, Then the run still finishes and nothing leaks.
func TestStreamConsumerReadsNothingStillFinishes(t *testing.T) {
	p := streamPrepared(t, &burstRunner{n: 3 * streamBuffer})
	base := runtime.NumGoroutine()
	ch := streamPreparedRun(&Client{}, p)
	// Under suite load other tests' goroutines can drop the count early, so wait for the full buffer too.
	for end := time.Now().Add(5 * time.Second); len(ch) < streamBuffer && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	waitGoroutines(t, base)
	if len(ch) != streamBuffer {
		t.Errorf("buffered = %d, want a full %d (oldest dropped)", len(ch), streamBuffer)
	}
}

// Given a full buffer with drops, Then the stream still ends in exactly one result, as its last event.
func TestStreamFullBufferDropsStillEndsInOneResult(t *testing.T) {
	notes := make(chan string, 2)
	c := &Client{Notify: func(s string) { notes <- s }}
	ch := streamPreparedRun(c, streamPrepared(t, &burstRunner{n: 2 * streamBuffer}))
	select { // the note fires as the run ends with nobody reading, so drops are certain
	case <-notes:
	case <-time.After(5 * time.Second):
		t.Fatal("no dropped-events note within 5s")
	}
	results, last := drainEvents(ch)
	if results != 1 || last.Type != wire.EventResult {
		t.Errorf("results = %d, last = %q, want one result last", results, last.Type)
	}
	if len(notes) != 0 {
		t.Errorf("extra notes = %d, want exactly one dropped-events note", len(notes)+1)
	}
}

// Given a consumer stalled past streamStall, Then the run's ctx is cancelled with errStreamAbandoned.
func TestStreamStalledConsumerCancelsRun(t *testing.T) {
	orig := streamStall
	streamStall = 20 * time.Millisecond
	t.Cleanup(func() { streamStall = orig })
	r := &burstRunner{n: streamBuffer, ctx: make(chan context.Context, 1)}
	ch := (&Client{}).streamWith(context.Background(), "fake", func(ctx context.Context, w wire.Sink) (wire.Result, error) {
		res, err := r.Run(ctx, RunOptions{}, w)
		for ctx.Err() == nil {
			_ = w.Emit(wire.Event{Type: wire.EventText})
			time.Sleep(5 * time.Millisecond)
		}
		return res, err
	})
	ctx := <-r.ctx
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ctx not cancelled by a stalled consumer")
	}
	if !errors.Is(context.Cause(ctx), errStreamAbandoned) {
		t.Errorf("cause = %v, want errStreamAbandoned", context.Cause(ctx))
	}
	drainEvents(ch)
}

// Given Prepare fails, Then the stream carries one failure result and closes.
func TestStreamPrepareFailureYieldsOneResult(t *testing.T) {
	setConfigDir(t)
	ch := (&Client{}).Stream(context.Background(), RunOptions{Agent: "no-such-agent", Prompt: "p"})
	results, last := drainEvents(ch)
	if results != 1 || last.Type != wire.EventResult || last.Status != wire.StatusError {
		t.Errorf("results = %d, last = %+v, want one error result", results, last)
	}
}

// The chanSink queues no progress mark for a Client stream.
func TestChanSinkDropsProgressMark(t *testing.T) {
	cs := newChanSink(func(error) {})
	_ = cs.Emit(wire.Event{Type: wire.EventProgress})
	if len(cs.ch) != 0 {
		t.Errorf("stream queued %d events; want none", len(cs.ch))
	}
}

// RunOptions.Lead must reach the router pick: a lead naming no registry row is skipped by name.
func TestClientRouteCarriesLeadToThePick(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	c := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	chain, err := c.route(t.Context(), nil, RunOptions{Agent: "codex", Prompt: "x", Lead: "no-such-row", Fallback: router.FallbackAll})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(chain.Skipped, "\n"), "no-such-row") {
		t.Fatalf("skipped = %q, want a skip naming the lead row no-such-row", chain.Skipped)
	}
}

// blocker is a regular file, so no directory can be created beneath it.
func blocker(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// Given a pack file that cannot be written, Then Exec fails and the claimed record leaves running.
func TestExecPackWriteFailureFinishesRecord(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
	p := &Prepared{Record: rec, pack: &packFile{path: filepath.Join(blocker(t), "pack.md"), body: "x"}}
	res, err := (&Client{}).Exec(context.Background(), p, wire.Discard, RunRecordedOpts{})
	if err == nil || res.Status != wire.StatusError {
		t.Fatalf("Exec = %v/%v, want the write error", res.Status, err)
	}
	got, lerr := LoadRecord(rec.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.Status != "error" {
		t.Errorf("record status = %q, want error", got.Status)
	}
}

// Given a tracked file and a tracked .env, Prepare packs the file only and records the task, not the bytes.
func TestPreparePacksAndKeepsRecordCompact(t *testing.T) {
	setupWorkdirFixture(t)
	dir := gitRepoWith(t, map[string]string{"main.go": "package main // NEEDLE-1", ".env": "TOKEN=hunter2"})
	var notes []string
	client := NewClient(router.RuleClassifier{}.Alias, nil, func(s string) { notes = append(notes, s) })
	p, err := client.Prepare(context.Background(), RunOptions{Agent: "opencode", Prompt: "explain", WorkDir: dir,
		Fallback: router.FallbackNone, Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if !strings.Contains(p.Opts.Prompt, "NEEDLE-1") || strings.Contains(p.Opts.Prompt, "hunter2") {
		t.Errorf("prompt must hold main.go and not .env:\n%s", p.Opts.Prompt)
	}
	if p.Record.Request.Prompt != "explain" || p.Record.Request.Pack == nil {
		t.Errorf("record = %q pack=%v, want the bare task and the pack spec", p.Record.Request.Prompt, p.Record.Request.Pack)
	}
}

func TestPrepareRefusesPackWithHost(t *testing.T) {
	setupWorkdirFixture(t)
	client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	_, err := client.Prepare(context.Background(), RunOptions{Agent: "opencode", Prompt: "p", Host: "box",
		Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("Prepare() = %v, want a --host refusal", err)
	}
}

func gitRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	names := []string{}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	for _, args := range [][]string{{"init", "-q"}, append([]string{"add"}, names...)} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// Given a lead with a 1M window and a stub on a 200K model, an n between their budgets drops only the stub.
func TestFitChainSkipsSmallWindows(t *testing.T) {
	lead := Dispatch{Agent: namedEngine("claude"), Model: "claude-sonnet-5-5[1m]"}
	small := Dispatch{Agent: namedEngine("stub"), Model: "mystery-model"}
	chain := Chain{Dispatches: []Dispatch{lead, small}, Preflight: &Preflight{}, requested: "p:m"}
	lb, _ := packBudget(lead, RunOptions{}, false)
	sb, _ := packBudget(small, RunOptions{}, false)
	if sb >= lb {
		t.Fatalf("fixture: small budget %d must be under lead %d", sb, lb)
	}
	got := fitChain(chain, (lb+sb)/2, RunOptions{}, false)
	if len(got.Dispatches) != 1 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "stub") {
		t.Fatalf("chain = %+v, want only the lead and a skip naming stub", got)
	}
	if got.Preflight != chain.Preflight || got.requested != "p:m" {
		t.Errorf("fitChain dropped Preflight or requested: %+v", got)
	}
	if kept := fitChain(chain, sb-1, RunOptions{}, false); len(kept.Dispatches) != 2 {
		t.Errorf("a prompt under both budgets must keep both, got %+v", kept)
	}
}

func TestPackBudgetArgvCap(t *testing.T) {
	d := Dispatch{Agent: namedEngine("opencode"), Model: "claude-sonnet-5-5[1m]"}
	if got, note := packBudget(d, RunOptions{}, false); got != MaxPromptBytes || !strings.Contains(note, "argv prompt cap") {
		t.Errorf("budget = %d (%q), want MaxPromptBytes with a cap note", got, note)
	}
}

// A PackFile lead keeps the prompt to the instruction, turns tools on, and its file never re-enters a Collect.
func TestPackPromptFileTransport(t *testing.T) {
	setupWorkdirFixture(t)
	body := strings.Repeat("x", 300<<10)
	dir := gitRepoWith(t, map[string]string{"big.txt": body, ".env": "TOKEN=hunter2"})
	lead := Dispatch{Agent: namedEngine("gemini"), Model: "claude-sonnet-5-5[1m]"}
	p := &Prepared{Opts: RunOptions{Prompt: "find it", WorkDir: dir, Pack: &pack.Spec{Dirs: []string{"."}, MaxFileBytes: 400 << 10}},
		Chain: Chain{Dispatches: []Dispatch{lead}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(p.Opts.Prompt) > 1024 || !strings.Contains(p.Opts.Prompt, p.pack.path) || !p.Opts.Tools || !p.Record.Request.Tools {
		t.Fatalf("prompt = %q tools=%v, want a short instruction naming %s with tools on", p.Opts.Prompt, p.Opts.Tools, p.pack.path)
	}
	if len(p.pack.body) < 300<<10 || strings.Contains(p.pack.body, "hunter2") {
		t.Fatalf("pack body = %d bytes, want the 300 KB file and no .env", len(p.pack.body))
	}
	if err := p.pack.write(); err != nil {
		t.Fatal(err)
	}
	got, err := pack.Collect(context.Background(), dir, []string{"."}, nil, 100, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Included {
		if strings.HasPrefix(f.Path, ".claude-code-pack-") {
			t.Errorf("Collect picked up the pack file %s", f.Path)
		}
	}
	p.pack.remove()
	if _, err := os.Stat(p.pack.path); !os.IsNotExist(err) {
		t.Errorf("pack file still present after remove: %v", err)
	}
}

// Given a gemini lead (pack read from a file with tools), --read-only keeps the run: agy enforces it with deny rules.
func TestPackPromptKeepsReadOnlyOnFileRoute(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	o := RunOptions{Prompt: "p", WorkDir: dir, ReadOnly: true, Pack: &pack.Spec{Dirs: []string{"."}}}
	p := &Prepared{Opts: o, Chain: Chain{Dispatches: []Dispatch{{Agent: namedEngine("gemini")}}}}
	if err := NewClient(router.RuleClassifier{}.Alias, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatalf("packPrompt() = %v, want the read-only file route accepted", err)
	}
	defer p.pack.remove()
	if !p.Opts.Tools || !p.Opts.ReadOnly {
		t.Fatalf("opts tools=%v read-only=%v, want both on", p.Opts.Tools, p.Opts.ReadOnly)
	}
}

// Given a relative --workdir, the file-route pack is staged under the state pack dir, never inside the workdir.
func TestPackPromptStagesPackOutsideWorkdir(t *testing.T) {
	setConfigDir(t)
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	t.Chdir(dir)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{Opts: RunOptions{Prompt: "p", WorkDir: "sub", Pack: &pack.Spec{Dirs: []string{"."}}},
		Chain: Chain{Dispatches: []Dispatch{{Agent: namedEngine("gemini")}}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(paths.PackDir(), pack.FilePrefix+"id1.md"); p.pack.path != want || !strings.Contains(p.Opts.Prompt, want) {
		t.Fatalf("pack path = %q, prompt = %q, want %q", p.pack.path, p.Opts.Prompt, want)
	}
	if strings.HasPrefix(p.pack.path, dir) {
		t.Fatalf("pack path %q is inside the repo %q", p.pack.path, dir)
	}
}

// namedEngine is an Engine that only has a name; the pack tests never run it.
type namedEngine string

func (s namedEngine) Name() string     { return string(s) }
func (s namedEngine) Caps() Caps       { return Caps{} }
func (s namedEngine) Available() error { return nil }
func (s namedEngine) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}

// A direct run whose auth fails after output started names the static token and routed default; a routed run does not.
func TestRunDirectAuthFailureNamesRoutedDefault(t *testing.T) {
	setConfigDir(t)
	for _, direct := range []bool{true, false} {
		f := &fakeRunner{caps: Caps{Direct: Native}, write: "partial", errs: []error{errors.New("HTTP 401 unauthorized")}}
		_, err := Run(context.Background(), f, RunOptions{Prompt: "p", Direct: direct}, wire.Discard, nil)
		if err == nil {
			t.Fatalf("direct=%v: want an error", direct)
		}
		if got := strings.Contains(err.Error(), "drop --direct"); got != direct {
			t.Errorf("direct=%v: hint present = %v: %v", direct, got, err)
		}
	}
}

// withDefaults reads AGENT_ERROR_FALLBACK as a mode: 1 is all, a mode is itself, unset is none, junk is refused.
func TestWithDefaultsFallbackFromEnv(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", router.FallbackNone}, {"1", router.FallbackAll}, {"yes", router.FallbackAll}, {"none", router.FallbackNone},
		{"pin", router.FallbackPin}, {"agents", router.FallbackAgents}, {"all", router.FallbackAll}, {"maybe", ""},
	} {
		t.Run(tc.env, func(t *testing.T) {
			hermetic(t, "")
			o, err := (&Client{}).withDefaults(RunOptions{Values: proc.Values{router.EnvFallback: tc.env}})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("%q accepted as %q", tc.env, o.Fallback)
				}
				return
			}
			if err != nil || o.Fallback != tc.want {
				t.Fatalf("%q = %q, %v; want %q", tc.env, o.Fallback, err, tc.want)
			}
		})
	}
}

func TestRateLimitYieldsBeforeBackoff(t *testing.T) {
	setConfigDir(t)
	for _, alternative := range []string{"workspace", "ladder", "none"} {
		t.Run(alternative, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				retry.ResetCooldowns()
				t.Cleanup(retry.ResetCooldowns)
				limited := errors.New("HTTP 429 rate limited")
				f := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{limited, limited, limited}}
				req := RunOptions{Prompt: "p"}
				if alternative == "workspace" {
					req.YieldFallback = func(res wire.Result, err error) bool { return HopFailed(res, err) }
				}
				c := chainOf([]Engine{f})
				if alternative == "ladder" {
					c = chainOf([]Engine{f, &fakeRunner{name: "gemini", errs: []error{nil}}})
				}
				start := time.Now()
				_, _ = runWithFallback(t.Context(), c, req, wire.Discard, nil)
				wait := time.Since(start)
				t.Logf("attempts=%d wait=%s", len(f.seen), wait)
				if alternative == "none" {
					if len(f.seen) != 3 || wait < 60*time.Second || wait > 90*time.Second {
						t.Errorf("no alternative must retain three attempts and 60–90s backoff")
					}
				} else if len(f.seen) != 1 || wait != 0 {
					t.Errorf("available fallback must run before any backoff")
				}
			})
		})
	}
}

func TestRunBudgetsFromOverlay(t *testing.T) {
	for _, tc := range []struct {
		agent, doc string
		bad        bool
	}{
		{"claude", `{"budgetUSD":0.0001}`, false},
		{"copilot", `{"budgetCredits":30}`, false},
		{"codex", `{"budgetUSD":1}`, true},
		{"claude", `{"budgetCredits":30}`, true},
	} {
		o, err := (&Client{}).withDefaults(RunOptions{Agent: tc.agent, Fallback: "none",
			Values: proc.Values{"COWORKER_OVERLAY": tc.doc}})
		if tc.bad && err != nil {
			code, ok := err.(interface{ ExitCode() int })
			if !ok || code.ExitCode() != 2 {
				t.Fatalf("budget refusal must exit 2: %v", err)
			}
		}
		if (err != nil) != tc.bad {
			t.Fatalf("%+v: %v", tc, err)
		}
		if !tc.bad && o.BudgetUSD == nil && o.BudgetCredits == nil {
			t.Fatal("budget lost")
		}
	}
}

type wordDriver struct {
	fixtureDriver
	err error
}

func (w wordDriver) ResolveModelWord(_ context.Context, m string) (string, error) {
	if m == "gpt" {
		return "gpt-9", w.err
	}
	return m, nil
}

// The run path resolves a family word like the label does; a driver without the resolver keeps the typed string.
func TestPrepareResolvesModelWordForResolverDriver(t *testing.T) {
	setupWorkdirFixture(t)
	for _, tc := range []struct {
		agent, want string
		err         error
		host        string
	}{{"copilot", "gpt-9", nil, ""}, {"opencode", "gpt", nil, ""}, {"copilot", "gpt", nil, "box"}, {"copilot", "", errors.New("cold"), ""}} {
		registryMu.Lock()
		orig := drivers["copilot"]
		drivers["copilot"] = wordDriver{fixtureDriver{NewBaseEngine("copilot")}, tc.err}
		registryMu.Unlock()
		client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
		p, err := client.Prepare(context.Background(), RunOptions{Agent: tc.agent, Prompt: "p", Model: "gpt", Host: tc.host,
			Fallback: router.FallbackNone}, nil)
		registryMu.Lock()
		drivers["copilot"] = orig
		registryMu.Unlock()
		var ue clientUsageError
		switch {
		case tc.err != nil && (!errors.As(err, &ue) || !strings.Contains(err.Error(), ErrModelRoute.Error())):
			t.Fatalf("%+v: err = %v, want a model-route usage error", tc, err)
		case tc.err == nil && tc.host == "" && (err != nil || p.Opts.Model != tc.want):
			t.Fatalf("%+v: err = %v, want model %q", tc, err, tc.want)
		}
	}
}

func TestRunOptionsEffectiveRoute(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"work": {}}}
	for name, tc := range map[string]struct {
		o    RunOptions
		want [3]string
	}{
		"route wins":      {withRoute(RunOptions{Provider: "x"}, "github", "p"), [3]string{"github", "p", ""}},
		"legacy provider": {RunOptions{Provider: "work", Account: "me"}, [3]string{"", "work", "me"}},
		"prefixed model":  {RunOptions{Model: "work:sonnet"}, [3]string{"", "work", ""}},
		"none":            {RunOptions{Model: "sonnet"}, [3]string{}},
	} {
		if r := tc.o.EffectiveRoute(reg); [3]string{r.Provider, r.Profile, r.Account} != tc.want {
			t.Errorf("%s: got %+v, want %v", name, r, tc.want)
		}
	}
}

func withRoute(o RunOptions, provider, profile string) RunOptions {
	o.Route.Provider, o.Route.Profile = provider, profile
	return o
}

type streamThenHang struct{ stubEngine }

func (streamThenHang) Run(ctx context.Context, _ RunOptions, w wire.Sink) (wire.Result, error) {
	w.Emit(wire.Event{Type: wire.EventText, Text: "half an answer"})
	<-ctx.Done()
	return wire.Result{}, ctx.Err()
}

// A run cut at its deadline after streaming keeps that text and reads partial, not stalled.
func TestRunDeadlineAfterStreamIsPartial(t *testing.T) {
	setConfigDir(t)
	eng := streamThenHang{stubEngine{name: "codex", caps: Caps{Tools: Native}}}
	res, err := Run(context.Background(), eng, RunOptions{Prompt: "p", Tools: true, Timeout: 20 * time.Millisecond}, wire.Discard, nil)
	if err == nil || res.Status != wire.StatusPartial || res.Answer != "half an answer" {
		t.Fatalf("status=%q answer=%q err=%v, want partial with the streamed text", res.Status, res.Answer, err)
	}
}
