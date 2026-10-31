package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// fakeLiveClaude is a shell claude that holds stdin open and answers each stream-json user turn with a result.
const fakeLiveClaude = `#!/bin/sh
mem=none
while IFS= read -r line; do
  p=$(printf '%s' "$line" | sed -n 's/.*"content":"\([^"]*\)".*/\1/p')
  case "$p" in
    remember*) mem=${p#remember }; ans=ok ;;
    slow*) : > "$CLAUDE_CONFIG_DIR/slow-started"; while [ ! -e "$CLAUDE_CONFIG_DIR/slow-go" ]; do sleep 0.02; done; ans=slow ;;
    boom*) printf '{"type":"error","message":"kaboom"}\n'; ans=late ;;
    *) ans="$mem pid $$" ;;
  esac
  printf '{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$ans"
  printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","session_id":"s1","usage":{"input_tokens":1,"output_tokens":1}}\n' "$ans"
done
`

// socketAccepts dials rather than stats: the socket file exists a moment before the holder listens on it.
func socketAccepts(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// liveEnv gives the test a short /tmp config dir (a unix socket path caps at 104 bytes on macOS, 108 on Linux) holding the fake claude.
func liveEnv(t *testing.T) { liveEnvScript(t, fakeLiveClaude) }

func liveEnvScript(t *testing.T, script string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	home := filepath.Join(dir, "home") // the agy target needs a Google sign-in marker; never read the developer's
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// startHolder claims a live record and runs ServeLive on it; the returned func waits for the holder and its error.
func startHolder(t *testing.T, ctx context.Context, idle time.Duration) (string, func() error) {
	t.Helper()
	return startHolderAs(t, ctx, "claude", idle)
}

func startHolderAs(t *testing.T, ctx context.Context, name string, idle time.Duration) (string, func() error) {
	t.Helper()
	rec := ClaimRecord(name, RunOptions{Timeout: idle}, time.Now())
	rec.Live, rec.PID = true, os.Getpid()
	rec.Request.Launcher = testLauncher{}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	done, id := make(chan error, 1), rec.ID
	go func() { done <- ServeLive(ctx, &rec, nil) }()
	for range 200 {
		if socketAccepts(LiveSocket(id)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, err := os.Stat(LiveSocket(id)); err != nil {
		select {
		case serveErr := <-done:
			t.Fatalf("holder did not open its socket: %v (serve: %v)", err, serveErr)
		default:
			t.Fatalf("holder did not open its socket: %v", err)
		}
	}
	return id, func() error { return <-done }
}

type collect struct {
	mu  sync.Mutex
	got []Event
}

func (c *collect) Emit(ev Event) error {
	c.mu.Lock()
	c.got = append(c.got, ev)
	c.mu.Unlock()
	return nil
}

// Two turns recall each other on one claude process, a concurrent send is refused, and cancel ends the session.
func TestLiveSessionTurnsRefusalAndCancel(t *testing.T) {
	liveEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	if info, err := os.Stat(LiveSocket(id)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v; want mode 0600", info, err)
	}
	var sink collect
	env, err := SendLive(context.Background(), id, "remember 42", &sink)
	if err != nil || env.Status != StatusOK || env.Answer != "ok" {
		t.Fatalf("turn 1 = %+v, %v", env, err)
	}
	if first := sink.got[0].Type; first != EventStart {
		t.Errorf("first event = %s, want start", first)
	}
	env2, err := SendLive(context.Background(), id, "what number", Discard)
	if err != nil || !strings.HasPrefix(env2.Answer, "42 pid ") {
		t.Fatalf("turn 2 = %+v, %v (the second turn must recall the first)", env2, err)
	}
	slow := make(chan error, 1)
	go func() { _, e := SendLive(context.Background(), id, "slow", Discard); slow <- e }()
	started := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "slow-started")
	for range 400 {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := SendLive(context.Background(), id, "second", Discard); err != ErrLiveBusy {
		t.Errorf("concurrent send = %v, want ErrLiveBusy", err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "slow-go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-slow; err != nil {
		t.Errorf("the in-flight turn must finish: %v", err)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LiveSocket(id)); !os.IsNotExist(err) {
		t.Errorf("socket must be removed on exit: %v", err)
	}
	got, _ := LoadRecord(id)
	if got.Status != "error" || got.Error != "cancelled" {
		t.Errorf("record = %s %q, want error cancelled", got.Status, got.Error)
	}
}

// An idle session ends as ok after the short override; a send after that is refused as ended.
func TestLiveIdleTimeoutEndsOK(t *testing.T) {
	liveEnv(t)
	id, wait := startHolder(t, context.Background(), 300*time.Millisecond)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadRecord(id); got.Status != "ok" {
		t.Errorf("idle end status = %s, want ok", got.Status)
	}
	if _, err := SendLive(context.Background(), id, "x", Discard); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("send after idle = %v, want an ended error", err)
	}
}

// A fifth session is refused; the cap counts only live, running records.
func TestLiveCapOfFour(t *testing.T) {
	liveEnv(t)
	for range MaxLiveSessions {
		rec := ClaimRecord("claude", RunOptions{}, time.Now())
		rec.Live, rec.PID = true, os.Getpid()
		_ = rec.Save()
	}
	if err := CheckLiveCap(""); err == nil {
		t.Fatal("cap must refuse a fifth session")
	}
	dead := ClaimRecord("claude", RunOptions{}, time.Now())
	dead.Live, dead.Status = true, "ok"
	_ = dead.Save()
	if n := LiveCount(""); n != MaxLiveSessions {
		t.Errorf("LiveCount = %d, want %d (finished sessions do not count)", n, MaxLiveSessions)
	}
}

// fakeNoisyClaude writes 200 KB to stdout before it reads stdin, so an undrained stdout blocks claude and,
// with a large prompt, the holder's stdin write too.
const fakeNoisyClaude = `#!/bin/sh
printf '{"type":"system","subtype":"noise","message":"%s"}\n' "$(head -c 200000 /dev/zero | tr '\0' x)"
` + fakeLiveClaude

// A large prompt sent while claude's between-turn output fills the stdout pipe must not deadlock the holder.
func TestLiveLargePromptWithPendingOutputDoesNotDeadlock(t *testing.T) {
	liveEnvScript(t, fakeNoisyClaude)
	id, wait := startHolder(t, context.Background(), 300*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := SendLive(context.Background(), id, strings.Repeat("y", 200000), Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("send deadlocked: stdin write blocked behind an undrained stdout")
	}
	if err := wait(); err != nil { // the idle timer must still run and end the session
		t.Fatal(err)
	}
}

// A turn that fails with a top-level error must never leave its late result to answer the next send.
func TestLiveTurnErrorNeverLeaksIntoNextTurn(t *testing.T) {
	liveEnv(t)
	id, wait := startHolder(t, context.Background(), 5*time.Second)
	if _, err := SendLive(context.Background(), id, "boom", Discard); err != nil {
		t.Logf("failing turn: %v", err)
	}
	env, err := SendLive(context.Background(), id, "remember 7", Discard)
	if err == nil && env.Answer == "late" {
		t.Fatalf("next send got the previous turn's answer %q", env.Answer)
	}
	_ = wait()
}

// With three slots held, two contenders racing for the last one: exactly one wins, the loser is refused.
func TestLiveSlotLastSlotGoesToExactlyOne(t *testing.T) {
	liveEnv(t)
	for range MaxLiveSessions - 1 {
		if _, err := AcquireLiveSlot(); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() { <-start; _, err := AcquireLiveSlot(); errs <- err }()
	}
	close(start)
	won := 0
	for range 2 {
		if err := <-errs; err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d contenders took the last slot, want exactly 1", won)
	}
}

// A released slot is reusable; a held one is not.
func TestLiveSlotReleaseFreesIt(t *testing.T) {
	liveEnv(t)
	var rel []func()
	for range MaxLiveSessions {
		r, err := AcquireLiveSlot()
		if err != nil {
			t.Fatal(err)
		}
		rel = append(rel, r)
	}
	if _, err := AcquireLiveSlot(); err == nil {
		t.Fatal("a fifth slot must be refused")
	}
	rel[0]()
	if _, err := AcquireLiveSlot(); err != nil {
		t.Errorf("released slot not reusable: %v", err)
	}
}

// fakeLiveAgy is a shell agy: one init, then per NDJSON turn an agent_response delta and a result whose usage is
// cumulative over the conversation, as the real agy reports it.
const fakeLiveAgy = `#!/bin/sh
printf '{"event":"init","conversation_id":"c1"}\n'
mem=none
n=0
while IFS= read -r line; do
  n=$((n + 1))
  m=$(printf '%s' "$line" | sed -n 's/.*remember \([0-9][0-9]*\).*/\1/p')
  if [ -n "$m" ]; then mem=$m; ans=ok; else ans="$mem"; fi
  printf '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"%s"}}\n' "$ans"
  printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"%s","usage":{"input_tokens":%d,"output_tokens":%d}}}\n' "$ans" $((n * 10)) $((n * 2))
done
`

// An agy session keeps its conversation across turns, reports per-turn usage, refuses a concurrent send, and cancels.
func TestLiveAgySession(t *testing.T) {
	liveEnv(t)
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "gemini"), []byte(fakeLiveAgy), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolderAs(t, ctx, "gemini", time.Minute)
	if _, err := os.Stat(LiveSocket(id)); err != nil {
		cancel()
		t.Fatalf("holder did not open its socket: %v (%v)", err, wait())
	}
	env, err := SendLive(context.Background(), id, "remember 42", Discard)
	if err != nil || env.Status != StatusOK || env.Answer != "ok" || env.Agent != "gemini" {
		t.Fatalf("turn 1 = %+v, %v", env, err)
	}
	env2, err := SendLive(context.Background(), id, "what number", Discard)
	if err != nil || env2.Answer != "42" {
		t.Fatalf("turn 2 = %+v, %v (the second turn must recall the first)", env2, err)
	}
	if env2.Usage.InputTokens != 10 || env2.Usage.OutputTokens != 2 {
		t.Errorf("turn 2 usage = %+v, want the turn's own delta 10/2, not agy's cumulative 20/4", env2.Usage)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	got, _ := LoadRecord(id)
	if got.Status != "error" || got.Error != "cancelled" || got.Agent != "gemini" || got.SessionID != "c1" {
		t.Errorf("record = %s %q agent=%s session=%q, want error cancelled gemini c1", got.Status, got.Error, got.Agent, got.SessionID)
	}
}

// An agent without a live mode is refused before anything starts.
func TestServeLiveRejectsAgentWithoutLive(t *testing.T) {
	liveEnv(t)
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	rec.Live = true
	if err := ServeLive(context.Background(), &rec, nil); err == nil || !strings.Contains(err.Error(), "no live mode") {
		t.Errorf("ServeLive(codex) = %v, want a no-live-mode error", err)
	}
}

// fakeCostClaude answers each turn with a result whose total_cost_usd accumulates 0.5 per turn, as claude's does.
const fakeCostClaude = `#!/bin/sh
n=0
while IFS= read -r line; do
  n=$((n + 1))
  printf '{"type":"result","subtype":"success","is_error":false,"result":"r","session_id":"s1","total_cost_usd":%s,"usage":{"input_tokens":1,"output_tokens":1}}\n' "$(echo "$n" | awk '{print $1 * 0.5}')"
done
`

// Claude's total_cost_usd accumulates over the process; each live turn must report only its own cost.
func TestLiveClaudeCostIsPerTurn(t *testing.T) {
	liveEnvScript(t, fakeCostClaude)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	defer func() { cancel(); _ = wait() }()
	for turn := 1; turn <= 3; turn++ {
		env, err := SendLive(context.Background(), id, "x", Discard)
		if err != nil || env.Usage.CostUSD != 0.5 {
			t.Fatalf("turn %d = cost %v, %v; want the turn's own 0.5", turn, env.Usage.CostUSD, err)
		}
	}
}

// fakeErrAgy fails the turn "fail" with an ERROR result and answers everything else.
const fakeErrAgy = `#!/bin/sh
printf '{"event":"init","conversation_id":"c1"}\n'
while IFS= read -r line; do
  case "$line" in
    *fail*) printf '{"event":"result","result":{"conversation_id":"c1","status":"ERROR","error":"upstream failed"}}\n' ;;
    *) printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"%s","usage":{"input_tokens":50,"output_tokens":5}}}\n' "$(printf '%s' "$line" | tr -d '\\"' | head -c 3000 | sed 's/.*content://')" ;;
  esac
done
`

func startAgy(t *testing.T, script string, req RunOptions) (string, func(), func() error) {
	t.Helper()
	liveEnv(t)
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "gemini"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := ClaimRecord("gemini", req, time.Now())
	rec.Live, rec.PID = true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeLive(ctx, &rec, nil) }()
	for range 200 {
		if socketAccepts(LiveSocket(rec.ID)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return rec.ID, cancel, func() error { return <-done }
}

// An agy ERROR result fails that turn only: the session stays usable for the next one.
func TestLiveAgyErrorResultKeepsSession(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeErrAgy, RunOptions{Timeout: time.Minute})
	defer func() { cancel(); _ = wait() }()
	env, err := SendLive(context.Background(), id, "fail now", Discard)
	if err == nil && env.Status != StatusError {
		t.Fatalf("failing turn = %+v, %v; want an error status", env, err)
	}
	if env2, err := SendLive(context.Background(), id, "again", Discard); err != nil || env2.Status != StatusOK {
		t.Fatalf("turn after an ERROR result = %+v, %v; the session must survive", env2, err)
	}
}

// System prompts are emulated on gemini live as the one-shot path does: prepended to the first turn only.
func TestLiveAgySystemPromptOnFirstTurn(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeErrAgy, RunOptions{Timeout: time.Minute, AppendSystemPrompt: "SPEAK-FRENCH"})
	defer func() { cancel(); _ = wait() }()
	e1, err := SendLive(context.Background(), id, "hello", Discard)
	if err != nil || !strings.Contains(e1.Answer, "SPEAK-FRENCH") {
		t.Fatalf("turn 1 = %+v, %v; want the system prompt in the first turn", e1, err)
	}
	e2, err := SendLive(context.Background(), id, "hello", Discard)
	if err != nil || strings.Contains(e2.Answer, "SPEAK-FRENCH") {
		t.Fatalf("turn 2 = %+v, %v; the system prompt must not repeat", e2, err)
	}
}

// A resumed agy conversation reports no usage for its first turn (the history is unknown), then per-turn deltas.
func TestLiveAgyResumedUsageBaseline(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeCumAgy, RunOptions{Timeout: time.Minute, SessionID: "old"})
	defer func() { cancel(); _ = wait() }()
	e1, err := SendLive(context.Background(), id, "a", Discard)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if e1.Usage != nil {
		t.Errorf("first resumed turn usage = %+v, want none (unknown), not the history", e1.Usage)
	}
	e2, err := SendLive(context.Background(), id, "b", Discard)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if e2.Usage == nil || e2.Usage.InputTokens != 10 {
		t.Errorf("second turn usage = %+v, want its own 10", e2.Usage)
	}
}

// fakeCumAgy reports cumulative usage that starts at 1000 (a resumed conversation) and grows 10 per turn.
const fakeCumAgy = `#!/bin/sh
n=1000
while IFS= read -r line; do
  n=$((n + 10))
  printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"r","usage":{"input_tokens":%d,"output_tokens":1}}}\n' "$n"
done
`

// A socket path over the unix limit fails with a message naming the length, not a bare "bind: invalid argument".
func TestListenLiveRefusesOverlongSocketPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), strings.Repeat("d", 100)))
	if _, err := listenLive("20261016T210512.123-claude"); err == nil || !strings.Contains(err.Error(), "unix limit") {
		t.Fatalf("listenLive = %v, want an over-the-unix-limit error", err)
	}
}
