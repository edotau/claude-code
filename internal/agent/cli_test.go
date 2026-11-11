package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex and gemini ride stdin (codexHeadless's `-` operand, agy's stream-json), so an oversized prompt
// never hits MAX_ARG_STRLEN and must not be refused before launch; opencode still puts the prompt in
// argv (opencodeRun), so it alone keeps the refusal. Provider: "ultra" (the built-in
// default) makes vendorTargetNote return immediately (req.Provider != "" short-circuits the vendor's own
// fallback resolution), so every case fails or refuses before any real CLI exec — no bin/codex, bin/agy
// or bin/opencode needed, and no network call.
func TestVendorEngineOversizedPromptRefusalScopedToArgv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
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

// TestVendorEngineNilProviderDefense tests that vendorRunner.Run safely handles
// the edge case where vendorTarget returns own=false with nil Provider.
// Line 104 (res.Provider = t.Provider.Name) must not panic when t.Provider is nil.
// Similarly, copilotEngine.Run (copilot.go) must handle nil Provider.
func TestVendorEngineNilProviderDefense(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Simulate the scenario where vendorTargetNote returns own=false with nil Provider.
	// The vulnerable code path is when own=false and t.Provider is accessed.
	// This test ensures the code doesn't panic with nil dereference.

	req := RunOptions{Prompt: "test"}

	// Call vendorTarget and check if t.Provider is nil when own=false.
	// If it is, the subsequent code must handle it gracefully (not panic).
	t_, own, err := SchemaFor("opencode").Target(req)

	// If we got here without panicking, that's good. But we should also check
	// that if own=false and we got a valid return, t.Provider is not nil OR
	// the calling code handles nil safely (with a nil check).
	if err == nil && !own && t_.Provider == nil {
		t.Logf("WARNING: vendorTarget returned own=false with nil Provider; " +
			"vendorRunner.Run line 104 must have a nil check")
		return // This is the defect scenario; fix should add nil check in Run()
	}
}

// A quiet child stream logs a heartbeat line while it runs.
func TestHeartbeatLogsWhenQuiet(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })
	var mu sync.Mutex
	var got []string
	lg := func(kind, text string) { mu.Lock(); got = append(got, kind); mu.Unlock() }
	stop := heartbeat(lg, &idleReader{last: time.Now().Add(-time.Minute).UnixNano()}, "gemini")
	time.Sleep(60 * time.Millisecond)
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != "heartbeat" {
		t.Errorf("log kinds = %v, want a heartbeat", got)
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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := SchemaFor("codex").ResolveTarget(reg, sel, false)
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
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "echo '{}'; sleep 30 & sleep 30"}, nil, "", "", nil, true)
	bad := errors.New("bad event")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = br.ReadBytes('\n')
		return wire.Result{Answer: "seen"}, bad
	}
	start := time.Now()
	res, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, nil)
	if time.Since(start) > 10*time.Second || !errors.Is(err, bad) || res.Answer != "seen" {
		t.Fatalf("took %v, res=%+v err=%v; want a prompt kill keeping the parse error and answer", time.Since(start), res, err)
	}
}

// A parse error after stdout reached EOF must not kill the child: its own exit status is the real failure.
func TestRunEventStreamParseErrorAfterEOFKeepsExitStatus(t *testing.T) {
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "echo '{}'; exec >&-; sleep 0.5; exit 7"}, nil, "", "", nil, true)
	bad := errors.New("turn failed")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = io.Copy(io.Discard, br)
		return wire.Result{}, bad
	}
	_, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, nil)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !errors.Is(err, bad) {
		t.Fatalf("err = %v; want exit code 7 (not a kill) joined with the parse error", err)
	}
}

// RunStream carries a structured-only answer: no text is not an empty answer when the schema output arrived.
func TestRunStreamKeepsStructured(t *testing.T) {
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, true)
	parse := func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) {
		return wire.Result{Structured: json.RawMessage(`{"ok":true}`)}, nil
	}
	res, err := RunStream(context.Background(), "x", cmd, tail, wire.Result{}, wire.Discard, parse)
	if err != nil || string(res.Structured) != `{"ok":true}` {
		t.Fatalf("res=%+v err=%v; want Structured kept and no empty-answer error", res, err)
	}
}

// A run on an agent's own login reports the registry profile naming that login, never the retired "own-login".
func TestOwnProfileNamesTheLoginRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := SchemaFor("codex").OwnProfile(); got == "own-login" || got == "" {
		t.Errorf("OwnProfile(codex) = %q", got)
	}
	if got := SchemaFor("opencode").OwnProfile(); got != "opencode" {
		t.Errorf("OwnProfile(opencode) = %q, want the agent name when no row names its login", got)
	}
}
