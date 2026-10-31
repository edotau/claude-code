package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// stallProvider is an opencode hub provider whose credential resolves through OPENCODE_API_KEY (no stored
// login needed), so the stall tests never depend on a real `opencode auth login`.
func stallProvider() *providers.Provider {
	return &providers.Provider{Name: "opencode", OpencodeID: "opencode",
		Models: map[string]string{models.Opus: "test-model", models.Sonnet: "test-model"}}
}

// opencodeNeverWrites answers --version instantly (so the credential/login probe never hangs) and
// otherwise sleeps without writing a single byte to stdout — the free-leg stall fixture.
const opencodeNeverWrites = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
sleep 5
`

// setupStallFixture isolates opencode's config/data dirs, installs the never-writes fake binary, and
// supplies a credential via the vault-key env var so Available()/Run() never touch a real login.
func setupStallFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	pathstest.FakeVendor(t, "opencode", opencodeNeverWrites)
	t.Setenv(OpencodeAPIKeyEnv, "test-key")
}

// TestWatchFreeLegOnlyWatchesFreeBilling: watchFreeLeg sets the stall watchdog on a hub leg only when the
// cell it runs bills free; any other billing (or any non-hub runner) passes through untouched — a paid
// opencode leg never gets the watchdog.
func TestWatchFreeLegOnlyWatchesFreeBilling(t *testing.T) {
	orig := freeFirstOutput
	freeFirstOutput = 50 * time.Millisecond
	t.Cleanup(func() { freeFirstOutput = orig })

	base := hubLeg{agent: "opencode", provider: stallProvider()}
	if r, ok := watchFreeLeg(base, router.BillSubscription).(hubLeg); !ok || r.firstOutput != 0 {
		t.Errorf("paid leg got a watchdog: %+v", r)
	}
	if r, ok := watchFreeLeg(base, router.BillCredits).(hubLeg); !ok || r.firstOutput != 0 {
		t.Errorf("credits-billed leg got a watchdog: %+v", r)
	}
	if r, ok := watchFreeLeg(base, router.BillFree).(hubLeg); !ok || r.firstOutput != freeFirstOutput {
		t.Errorf("free leg missing its watchdog: firstOutput=%v, want %v", r.firstOutput, freeFirstOutput)
	}

	// a non-hub runner (another vendor) must pass through unchanged, not panic on the type assertion.
	fr := &fakeRunner{name: "codex"}
	if r := watchFreeLeg(fr, router.BillFree); r != fr {
		t.Errorf("non-hub runner must pass through unchanged, got %v", r)
	}
}

// TestHubLegRunStallsFallsBackAndCoolsDown is the stall test's teeth: a free hub leg whose child never
// writes stdout fails fast with errStalled/KindFatal (not retried — Do's MaxAttempts would otherwise burn
// minutes), RunWithFallback falls through to the paid backup leg, and noteStall's markExhausted seam
// records opencode:zen-free exhausted for ~15 minutes, naming the stall in its reason.
func TestHubLegRunStallsFallsBackAndCoolsDown(t *testing.T) {
	setupStallFixture(t)
	origDeadline := freeFirstOutput
	freeFirstOutput = 50 * time.Millisecond
	t.Cleanup(func() { freeFirstOutput = origDeadline })

	var marked []string
	origMark := markExhausted
	markExhausted = func(account string, until, now time.Time, reason string) error {
		marked = append(marked, account+"|"+reason)
		if d := until.Sub(now); d < 14*time.Minute || d > 16*time.Minute {
			t.Errorf("markExhausted window = %v, want ~15m", d)
		}
		return nil
	}
	t.Cleanup(func() { markExhausted = origMark })

	free := watchFreeLeg(hubLeg{agent: "opencode", provider: stallProvider()}, router.BillFree)
	paid := &fakeRunner{name: "opencode", errs: []error{nil}}
	c := Chain{Want: "opencode", Legs: []Leg{
		{Agent: free, Account: "opencode:zen-free", Billing: router.BillFree},
		{Agent: paid, Account: "opencode:go", Billing: router.BillSubscription},
	}}

	start := time.Now()
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("a stalled free leg must fail fast on its own deadline, took %v", elapsed)
	}
	if err != nil || res.Answer != "a" {
		t.Fatalf("res=%+v err=%v, want the paid backup's answer with no error", res, err)
	}
	if !strings.Contains(res.FallbackReason, "free leg stalled") {
		t.Errorf("FallbackReason = %q, want it naming the stall", res.FallbackReason)
	}
	if len(marked) != 1 {
		t.Fatalf("markExhausted calls = %v, want exactly one (the free leg)", marked)
	}
	if !strings.HasPrefix(marked[0], "opencode:zen-free|") || !strings.Contains(marked[0], freeFirstOutput.String()) {
		t.Errorf("markExhausted = %q, want opencode:zen-free with the stall deadline in the reason", marked[0])
	}
}

// opencodeStepStartThenStalls prints only a bookkeeping step_start event (no text/reasoning/tool, no
// output tokens) and then sleeps without further output — proving step_start alone must never disarm
// the stall watchdog (isMeaningfulOpenCodeEvent must reject it).
const opencodeStepStartThenStalls = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"step_start"}'
sleep 5
`

// opencodeTextFast prints a real text event right away, then stays alive past the firstOutput deadline
// before exiting 0 — proving the watchdog actually disarmed (timer.Stop fired) rather than the test
// passing only because the child happened to exit before the deadline could ever fire.
const opencodeTextFast = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"text","part":{"text":"hi"}}'
sleep 0.6
`

// opencodeEmptyTextThenStalls prints a text event with an empty Part.Text (bookkeeping, not real output)
// and then sleeps without further output — proving an empty text event must never disarm the watchdog
// (isMeaningfulOpenCodeEvent must reject it on the TrimSpace(Part.Text) == "" gate).
const opencodeEmptyTextThenStalls = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"text","part":{"text":""}}'
sleep 5
`

// TestHubLegRunEmptyTextAloneStillStalls is the stall watchdog's teeth on the empty-text side: a child
// that only ever emits a text event with an empty Part.Text (never any non-empty text, reasoning, tool
// activity, or output tokens) must still stall and fail with errStalled.
func TestHubLegRunEmptyTextAloneStillStalls(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	pathstest.FakeVendor(t, "opencode", opencodeEmptyTextThenStalls)
	t.Setenv(OpencodeAPIKeyEnv, "test-key")
	orig := freeFirstOutput
	freeFirstOutput = 50 * time.Millisecond
	t.Cleanup(func() { freeFirstOutput = orig })

	l := hubLeg{agent: "opencode", provider: stallProvider(), firstOutput: freeFirstOutput}
	start := time.Now()
	res, err := l.Run(context.Background(), RunOptions{Prompt: "p"}, Discard)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("empty-text-only stall must still fire on firstOutput: took %v", elapsed)
	}
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want errStalled — an empty text event alone must not disarm the watchdog", err)
	}
	if res.Answer != "" {
		t.Errorf("a stalled leg must carry no answer, got %q", res.Answer)
	}
}

// TestHubLegRunStepStartAloneStillStalls is the stall watchdog's teeth on the event-type side: a child that
// only ever emits step_start (never text, reasoning, tool, or output tokens) must still stall and fail with
// errStalled — proving the watchdog disarms on meaningful model output, not on any stream activity at all.
func TestHubLegRunStepStartAloneStillStalls(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	pathstest.FakeVendor(t, "opencode", opencodeStepStartThenStalls)
	t.Setenv(OpencodeAPIKeyEnv, "test-key")
	orig := freeFirstOutput
	freeFirstOutput = 50 * time.Millisecond
	t.Cleanup(func() { freeFirstOutput = orig })

	l := hubLeg{agent: "opencode", provider: stallProvider(), firstOutput: freeFirstOutput}
	start := time.Now()
	res, err := l.Run(context.Background(), RunOptions{Prompt: "p"}, Discard)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("step_start-only stall must still fire on firstOutput: took %v", elapsed)
	}
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want errStalled — a step_start event alone must not disarm the watchdog", err)
	}
	if res.Answer != "" {
		t.Errorf("a stalled leg must carry no answer, got %q", res.Answer)
	}
}

// TestHubLegRunTextOutputDisarmsWatchdog proves the other side: a child that emits a real text event right
// away must never stall, even though it then stays alive past the firstOutput deadline — the child outlives
// the deadline on purpose, so a passing test proves the watchdog's timer.Stop actually fired rather than
// the test only passing because the child exited before the deadline could ever trigger.
func TestHubLegRunTextOutputDisarmsWatchdog(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	pathstest.FakeVendor(t, "opencode", opencodeTextFast)
	t.Setenv(OpencodeAPIKeyEnv, "test-key")
	orig := freeFirstOutput
	freeFirstOutput = 300 * time.Millisecond // headroom for a loaded box to start the fake child
	t.Cleanup(func() { freeFirstOutput = orig })

	l := hubLeg{agent: "opencode", provider: stallProvider(), firstOutput: freeFirstOutput}
	start := time.Now()
	res, err := l.Run(context.Background(), RunOptions{Prompt: "p"}, Discard)
	elapsed := time.Since(start)
	if errors.Is(err, errStalled) {
		t.Fatalf("a quick text event must disarm the watchdog, got errStalled: %v", err)
	}
	if err != nil {
		t.Fatalf("res=%+v err=%v, want no error", res, err)
	}
	if res.Answer != "hi" {
		t.Errorf("res.Answer = %q, want %q", res.Answer, "hi")
	}
	if elapsed <= freeFirstOutput {
		t.Fatalf("elapsed = %v, want > firstOutput (%v) — the child must outlive the deadline for this to be a real proof", elapsed, freeFirstOutput)
	}
}

// TestHubLegRunStallErrorIsFatalNotRetried proves hubLeg.Run's stall path on its own: no stdout within
// firstOutput returns a fresh *Error{Kind: KindFatal, Err: errStalled}, never wrapping ctx.Err directly —
// so errors.Is(err, errStalled) holds, and Retryable() is false (Do must not retry a dead free leg).
func TestHubLegRunStallErrorIsFatalNotRetried(t *testing.T) {
	setupStallFixture(t)
	orig := freeFirstOutput
	freeFirstOutput = 50 * time.Millisecond
	t.Cleanup(func() { freeFirstOutput = orig })

	l := hubLeg{agent: "opencode", provider: stallProvider(), firstOutput: freeFirstOutput}
	start := time.Now()
	res, err := l.Run(context.Background(), RunOptions{Prompt: "p"}, Discard)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stall must fire on firstOutput, not AttemptTimeout: took %v", elapsed)
	}
	if res.Answer != "" {
		t.Errorf("a stalled leg must carry no answer, got %q", res.Answer)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want it to carry an *Error", err)
	}
	if e.Kind != KindFatal || !errors.Is(err, errStalled) {
		t.Fatalf("err = %+v, want Kind=KindFatal wrapping errStalled", e)
	}
	if e.Retryable() {
		t.Error("a stall must never be retryable")
	}
}
