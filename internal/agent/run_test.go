package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	f := &noteRunner{fakeRunner{caps: Caps{Model: Native, SystemPrompt: Emulated}, errs: []error{nil}}}
	res, err := Run(context.Background(), f, RunOptions{Prompt: "p", SystemPrompt: "sys"}, wire.Discard, nil)
	if want := []string{"--system-prompt", "vendor-note"}; err != nil || !reflect.DeepEqual(res.Emulated, want) {
		t.Fatalf("Emulated=%v want %v err=%v", res.Emulated, want, err)
	}
}

// Streamed text or a tool call that may have had effects vetoes the retry: one attempt, Progressed set.
func TestRunProgressVetoesRetry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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

// A 402 (account exhausted: no credits/plan window left) is KindQuota and IsQuota-true but ModelDenied-false:
// a lower alias on the same account is just as exhausted, so Run must not demote and must return the error.
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	assertQuotaNoDemote(t, errors.New("HTTP 402 payment required: you have exceeded your monthly quota"))

	// A body carrying BOTH the 402/payment-required account marker AND a model-denied marker
	// ("insufficient_quota") is still account-exhausted: hardAccountRE vetoes ModelDenied, so Run must not
	// demote off a dead account on the strength of the model-denied marker alone.
	assertQuotaNoDemote(t, errors.New("402 payment required: insufficient_quota"))
}

// TestRunTimeoutEndsKindFatalRegardlessOfTools: Run() no longer special-cases a tools-on timeout (that
// branch was removed as redundant); Do's own AttemptTimeout-is-terminal path is the only one left, and it
// must classify a hung call to KindFatal wrapping context.DeadlineExceeded the same whether Tools was on.
func TestRunTimeoutEndsKindFatalRegardlessOfTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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
	StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-xyz")
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
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
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
	for _, r := range ReadOnlyDeny {
		if !slices.Contains(got.Permissions.Deny, r) {
			t.Errorf("deny %v lacks %s", got.Permissions.Deny, r)
		}
	}
	if out, dropped, _ := applyCaps(g.Caps(), got); !out.Tools || !out.ReadOnly || len(dropped) != 0 {
		t.Errorf("applyCaps: tools=%v read-only=%v dropped=%v, want both kept", out.Tools, out.ReadOnly, dropped)
	}
}

// fastDefaultBackoff shrinks DefaultPolicy's backoff for the test and restores it on cleanup.
func fastDefaultBackoff(t *testing.T) {
	t.Helper()
	b, m := retry.DefaultBaseDelay, retry.DefaultMaxDelay
	retry.DefaultBaseDelay, retry.DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { retry.DefaultBaseDelay, retry.DefaultMaxDelay = b, m })
}

func TestRunAuthRecoveryUsesCodexPinnedProvider(t *testing.T) {
	vaulttest.Use(t)
	fastDefaultBackoff(t)
	marker := filepath.Join(t.TempDir(), "refreshed")
	installRecoveryProfiles(t, marker)
	t.Setenv(providers.EnvSessionProfile, "regression-session")
	vaulttest.SetRecord(t, "codex", "profile", "regression-pinned")
	r := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("401 Unauthorized"), nil}}
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

// "<profile>:<alias>" resolves like --provider <profile> --model <alias>; anything else is left alone.
func TestResolveTargetProfileTierRewrite(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5","haiku":"claude-haiku-4-5"}}}}}`)
	_, want, err := resolveTarget(RunOptions{Provider: "dev", Model: "opus"})
	if err != nil || want.Provider == nil || want.Provider.Name != "dev" {
		t.Fatalf("baseline --provider dev --model opus = %+v, %v", want, err)
	}

	t.Run("dev:opus equals --provider dev --model opus", func(t *testing.T) {
		_, got, err := resolveTarget(RunOptions{Model: "dev:opus"})
		if err != nil || got.Provider == nil || got.Provider.Name != "dev" || got.Model != want.Model {
			t.Errorf("dev:opus = %+v (%v), want provider dev model %q", got, err, want.Model)
		}
	})
	t.Run("full id keeps its prior behaviour", assertFullIDTargetRewrite)
	t.Run("unknown row or non-alias is not rewritten", func(t *testing.T) { assertUnknownTargetRewrite(t, want.Model) })
	t.Run("the explicit backend is still seen", func(t *testing.T) {
		req := RunOptions{Model: "dev:opus"}
		if !ExplicitBackend(req) {
			t.Error("ExplicitBackend(dev:opus) must stay true")
		}
		if acct, _ := explicitAccount(&fakeRunner{name: "claude"}, req); acct != "databricks·dev" {
			t.Errorf("explicitAccount = %q, want databricks·dev", acct)
		}
	})
}

func assertFullIDTargetRewrite(t *testing.T) {
	t.Helper()
	_, viaSpec, err1 := resolveTarget(RunOptions{Model: "dev:claude-opus-5-5"})
	_, viaFlags, err2 := resolveTarget(RunOptions{Provider: "dev", Model: "claude-opus-5-5"})
	if err1 != nil || err2 != nil || viaSpec.Provider == nil || viaSpec.Provider.Name != "dev" ||
		viaSpec.Model != viaFlags.Model {
		t.Errorf("spec %+v (%v) vs flags %+v (%v)", viaSpec, err1, viaFlags, err2)
	}
}

func assertUnknownTargetRewrite(t *testing.T, model string) {
	t.Helper()
	_, bad, err := resolveTarget(RunOptions{Model: "unknownrow:opus"})
	if err == nil && bad.Provider != nil && bad.Provider.Name == "unknownrow" {
		t.Errorf("unknownrow:opus rewritten: %+v", bad)
	}
	_, nt, _ := resolveTarget(RunOptions{Model: "dev:notatier"})
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

// A vault overlay's default policy reaches the engine under the request: Deny holds, the request's allow and mode widen.
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
	if d := got.Decide("Bash", "git log && rm -rf x"); d != Deny {
		t.Errorf("the default Deny must hold against a compound command, got %v", d)
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

// A canonical Claude id takes the session's alias selection, so a CLAUDE_<TIER>_MODEL pin wins over the typed id.
func TestResolveTargetCanonicalIDTakesTheTierPin(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5-5"}}}}}`)
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "claude-sonnet-5")
	_, got, err := resolveTarget(RunOptions{Provider: "dev", Model: "claude-sonnet-5-5"})
	if err != nil || got.Model != "claude-sonnet-5" {
		t.Errorf("claude-sonnet-5-5 = %+v (%v), want the pinned claude-sonnet-5", got, err)
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
