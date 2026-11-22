package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex/gemini ride stdin so an oversized prompt is not refused; opencode keeps argv and refuses; "ultra" runs no CLI.
func TestVendorEngineOversizedPromptRefusalScopedToArgv(t *testing.T) {
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	big := strings.Repeat("x", MaxPromptBytes+10)

	for _, name := range []string{"codex", "gemini"} {
		t.Run(name+"/not-refused", func(t *testing.T) {
			_, err := newCLIEngine(name).Run(context.Background(), RunOptions{Prompt: big, Provider: "ultra"}, wire.Discard)
			if err == nil {
				t.Fatalf("%s: want a non-nil error (no route for a fake provider), got nil", name)
			}
			if errors.Is(err, ErrPromptTooLarge) {
				t.Fatalf("%s: err = %v, want anything but ErrPromptTooLarge (prompt rides stdin, not argv)", name, err)
			}
		})
	}

	t.Run("opencode/still-refused", func(t *testing.T) {
		_, err := newCLIEngine("opencode").Run(context.Background(), RunOptions{Prompt: big, Provider: "ultra"}, wire.Discard)
		var e *retry.Error
		if !errors.As(err, &e) || !errors.Is(e, ErrPromptTooLarge) {
			t.Fatalf("opencode: err = %v, want *Error wrapping ErrPromptTooLarge (prompt rides argv)", err)
		}
	})
}

// A quiet child stream logs a heartbeat line while it runs.
func TestHeartbeatLogsWhenQuiet(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })
	var mu sync.Mutex
	var got []string
	lg := func(kind, text string) { mu.Lock(); got = append(got, kind); mu.Unlock() }
	stop := heartbeat(lg, &idleReader{last: time.Now().Add(-time.Minute).UnixNano()}, "gemini", "")
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != "heartbeat" {
		t.Errorf("log kinds = %v, want a heartbeat", got)
	}
}

// A parser that panics still stops the heartbeat goroutine.
func TestRunEventStreamPanicStopsHeartbeat(t *testing.T) {
	hosttest.SkipOnWindows(t, "unix tools")
	ctx := wire.WithLog(context.Background(), wire.NewLogSink(filepath.Join(t.TempDir(), "d.log")))
	parse := func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) { panic("parse boom") }
	running := func() bool {
		buf := make([]byte, 1<<20)
		return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "agent.heartbeat.func")
	}
	cmd := exec.Command("sleep", "5")
	func() {
		defer func() { _ = recover() }()
		_, _ = RunEventStream(ctx, "x", cmd, nil, wire.Discard, parse, StreamOpts{})
	}()
	defer func() { _ = cmd.Process.Kill() }()
	for deadline := time.Now().Add(time.Second); running(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat goroutine still running after the parser panicked")
		}
	}
}

func TestVendorIsolationCaps(t *testing.T) {
	for name, want := range map[string][3]Level{
		"codex": {Native, Native, None}, "opencode": {None, Native, None}, "gemini": {None, None, Native}} {
		c := newCLIEngine(name).Caps()
		if got := [3]Level{c.NoPersist, c.SafeMode, c.DebugFile}; got != want {
			t.Errorf("%s caps = %v, want %v", name, got, want)
		}
	}
}

// vendorTargetBase honors the request's model alias; ResolveVendorTarget always returns the session's opus alias.
func TestVendorTargetBaseDiffersFromResolveTargetOnTier(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := SchemaFor("codex").ResolveTarget(context.Background(), RunOptions{}, reg, sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	bt, _, _, _, err := SchemaFor("codex").targetBase(RunOptions{Prompt: "p", Model: "haiku"})
	if err != nil {
		t.Fatalf("vendorTargetBase: %v", err)
	}
	if rt.Model == bt.Model {
		t.Errorf("expected the alias to differ, both %q", rt.Model)
	}
	if want := bt.Provider.Models["haiku"]; bt.Model != want {
		t.Errorf("base model = %q, want the haiku alias %q", bt.Model, want)
	}
}

// A parser failure kills the child at once: a child (or grandchild) holding stdout open never runs to --timeout.
func TestRunEventStreamParseErrorKillsChild(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	old := childWaitDelay
	childWaitDelay = 500 * time.Millisecond // a grandchild forked as the group kill lands survives until this
	t.Cleanup(func() { childWaitDelay = old })
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "echo '{}'; sleep 60 & sleep 60"}, nil, "", "", nil, true)
	bad := errors.New("bad event")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = br.ReadBytes('\n')
		return wire.Result{Answer: "seen"}, bad
	}
	start := time.Now()
	res, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, StreamOpts{})
	if time.Since(start) >= 30*time.Second || !errors.Is(err, bad) || res.Answer != "seen" {
		t.Fatalf("took %v, res=%+v err=%v; want a prompt kill keeping the parse error and answer", time.Since(start), res, err)
	}
}

// A parse error after stdout reached EOF must not kill the child: its own exit status is the real failure.
func TestRunEventStreamParseErrorAfterEOFKeepsExitStatus(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	release := filepath.Join(t.TempDir(), "release")
	script := "echo '{}'; exec >&-; while [ ! -e \"$1\" ]; do sleep 0.01; done; exit 7"
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", script, "sh", release}, nil, "", "", nil, true)
	bad := errors.New("turn failed")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = io.Copy(io.Discard, br)
		_ = os.WriteFile(release, nil, 0o600) // the child exits only after parse saw EOF
		return wire.Result{}, bad
	}
	_, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, StreamOpts{})
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !errors.Is(err, bad) {
		t.Fatalf("err = %v; want exit code 7 (not a kill) joined with the parse error", err)
	}
}

// RunStream carries a structured-only answer: no text is not an empty answer when the schema output arrived.
func TestRunStreamKeepsStructured(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, true)
	parse := func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) {
		return wire.Result{Structured: json.RawMessage(`{"ok":true}`)}, nil
	}
	res, err := RunStream(context.Background(), "x", cmd, tail, wire.Result{}, wire.Discard, parse)
	if err != nil || string(res.Structured) != `{"ok":true}` {
		t.Fatalf("res=%+v err=%v; want Structured kept and no empty-answer error", res, err)
	}
}

// The record's reported_model is only the vendor stream's id: the requested launch model never fills it.
func TestWithParsedReportedModelOnlyFromVendor(t *testing.T) {
	for _, c := range []struct{ streamed, want string }{{" served-x ", "served-x"}, {"", ""}} {
		res := withParsed(wire.Result{Model: "requested-y"}, wire.Result{Model: c.streamed})
		var rec Record
		rec.Finish(res, nil, 0)
		if rec.Reported != c.want {
			t.Errorf("streamed %q: reported_model = %q, want %q", c.streamed, rec.Reported, c.want)
		}
	}
}

// A run on an agent's own login reports the registry profile naming that login, never the retired "own-login".
func TestOwnProfileNamesTheLoginRow(t *testing.T) {
	setConfigDir(t)
	if got := SchemaFor("codex").OwnProfile(); got == "own-login" || got == "" {
		t.Errorf("OwnProfile(codex) = %q", got)
	}
	if got := SchemaFor("opencode").OwnProfile(); got != "opencode" {
		t.Errorf("OwnProfile(opencode) = %q, want the agent name when no row names its login", got)
	}
}

// --provider <profile> --model <alias> resolves to the row's alias; anything else is left alone.
func TestResolveTargetProfileTierRewrite(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5","haiku":"claude-haiku-4-5"}}}}}`)
	_, want, err := ResolveRequestTarget(RunOptions{Provider: "dev", Model: "opus"})
	if err != nil || want.Provider == nil || want.Provider.Name != "dev" {
		t.Fatalf("baseline --provider dev --model opus = %+v, %v", want, err)
	}

	t.Run("full id keeps its prior behaviour", assertFullIDTargetRewrite)
	t.Run("unknown row or non-alias is not rewritten", func(t *testing.T) { assertUnknownTargetRewrite(t, want.Model) })
	t.Run("an alias cell resolves on its row", func(t *testing.T) {
		_, at, err := ResolveRequestTarget(RunOptions{Model: "opus", CellRoute: rowRoute(nil, "dev")})
		if err != nil || at.Provider == nil || at.Provider.Name != "dev" || at.Model != want.Model {
			t.Errorf("an alias cell on dev = %+v, %v; want dev's opus alias %s", at, err, want.Model)
		}
	})
	t.Run("the explicit backend is still seen", func(t *testing.T) {
		req := RunOptions{Provider: "dev", Model: "opus"}
		if !explicitBackend(req) {
			t.Error("ExplicitBackend(--provider dev) must stay true")
		}
		if acct, _ := explicitAccount(&fakeRunner{name: "claude"}, req); acct != "databricks·dev" {
			t.Errorf("explicitAccount = %q, want databricks·dev", acct)
		}
	})
}

// A canonical Claude id takes the session's alias selection, so a CLAUDE_<TIER>_MODEL pin wins over the typed id.
func TestResolveTargetCanonicalIDTakesTheTierPin(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"DEV_TEST_KEY"},"models":{"opus":"claude-opus-5-5","sonnet":"claude-sonnet-5-5"}}}}}`)
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "claude-sonnet-5")
	_, got, err := ResolveRequestTarget(RunOptions{Provider: "dev", Model: "claude-sonnet-5-5"})
	if err != nil || got.Model != "claude-sonnet-5" {
		t.Errorf("claude-sonnet-5-5 = %+v (%v), want the pinned claude-sonnet-5", got, err)
	}
}

// An untyped opencode run on a Databricks row runs the brief's tier: a typo fix and an audit run different models.
func TestRoutedModelRunsTaskTier(t *testing.T) {
	setConfigDir(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	run := func(prompt, model string, cell bool) string {
		req := RunOptions{Prompt: prompt, Model: model, CellModel: cell}
		got, _, err := SchemaFor("opencode").routedModel(context.Background(), req, providers.Target{Provider: reg.Providers["dev"]}, false)
		if err != nil {
			t.Fatalf("routedModel(%q): %v", prompt, err)
		}
		return got.Model
	}
	trivial, hard := "Fix the typo in internal/agent/label.go.", "Audit the safety-scan rules; needs judgment."
	if a, b := run(trivial, "sonnet", true), run(hard, "sonnet", true); a == b {
		t.Errorf("trivial and audit briefs both run %s, want the tier to pick different models", a)
	}
	if a, b := run(trivial, "claude-opus-5-5", false), run(hard, "claude-opus-5-5", false); a != b {
		t.Errorf("a typed model ran %s and %s, want the typed model on both", a, b)
	}
	vaulttest.SetRecord(t, "opencode", "model", "databricks-claude-sonnet-5-5")
	if a, b := run(trivial, "", false), run(hard, "", false); a != b {
		t.Errorf("a pinned agent ran %s and %s, want its pin on both", a, b)
	}
}

// A typed family reaches the launch target concrete on Databricks and first-party rows; a concrete id reads no list.
func TestResolveInFamilyTargetsAreConcrete(t *testing.T) {
	vaulttest.Use(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6.2-sol"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serving.Save(serving.Served{Profile: "dev", FetchedAt: time.Now(), Models: []serving.ServedModel{
		{ID: "databricks-gpt-6-1-sol"}, {ID: "databricks-gpt-6-2-sol"}}}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	copilotauth.StubRead(t, func(string) []string {
		reads++
		return []string{"gpt-6.1-sol", "gpt-6.2-sol"}
	}, nil)
	reg := routeRegistry(t)
	for _, c := range []struct{ agent, row, want string }{
		{"codex", "dev", "databricks-gpt-6-2-sol"},
		{"codex", "chatgpt", "gpt-6.2-sol"},
		{"copilot", "aueric_boehcp", "gpt-6.2-sol"},
	} {
		_, tgt, err := resolveIn(reg, RunOptions{Agent: c.agent, Provider: c.row, Model: "sol"})
		if err != nil || tgt.Model != c.want {
			t.Errorf("%s --provider %s sol = %q, %v; want %s", c.agent, c.row, tgt.Model, err, c.want)
		}
	}
	if reads != 1 {
		t.Fatalf("github list reads = %d, want 1 for the one github family request", reads)
	}
	for _, m := range []string{"gpt-6.1-sol", "claude-opus-5.5", "opus"} {
		if _, tgt, err := resolveIn(reg, RunOptions{Agent: "copilot", Provider: "aueric_boehcp", Model: m}); err != nil || tgt.Model == "" {
			t.Errorf("concrete %s = %q, %v", m, tgt.Model, err)
		}
	}
	if reads != 1 {
		t.Errorf("a concrete id or alias read the list: reads = %d", reads)
	}
}

func TestEmptyOrWorked(t *testing.T) {
	for _, c := range []struct {
		name    string
		res     wire.Result
		toolRan bool
		ok      bool
	}{
		{"nothing", wire.Result{}, false, false},
		{"tool ran", wire.Result{}, true, true},
		{"file written", wire.Result{Written: []string{"/x"}}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := EmptyOrWorked("x", c.res, c.toolRan)
			if (err == nil) != c.ok || (c.ok && res.Answer != NoTextAnswer) {
				t.Fatalf("answer %q err %v; want ok=%v", res.Answer, err, c.ok)
			}
		})
	}
}

func TestIdleReaderStampsFirstEventOnce(t *testing.T) {
	r := &idleReader{src: eofReader{R: strings.NewReader("ab")}, start: time.Now().Add(-time.Second)}
	if got := r.stampFirst(nil); got != nil {
		t.Fatalf("no read yet: transport = %+v, want nil", got)
	}
	buf := make([]byte, 1)
	_, _ = r.Read(buf)
	first := r.stampFirst(nil)
	if first == nil || first.FirstEventMs < 1000 {
		t.Fatalf("first event ms = %+v, want >= 1000", first)
	}
	time.Sleep(2 * time.Millisecond)
	_, _ = r.Read(buf)
	if again := r.stampFirst(&wire.Transport{Attempts: 2}); again.FirstEventMs != first.FirstEventMs || again.Attempts != 2 {
		t.Fatalf("later read changed it: %+v vs %+v", again, first)
	}
	if kept := r.stampFirst(&wire.Transport{FirstEventMs: 7}); kept.FirstEventMs != 7 {
		t.Fatalf("parser value overwritten: %+v", kept)
	}
}
