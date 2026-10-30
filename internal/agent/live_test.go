package agent

import (
	"context"
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
    slow*) sleep 2; ans=slow ;;
    boom*) printf '{"type":"error","message":"kaboom"}\n'; ans=late ;;
    *) ans="$mem pid $$" ;;
  esac
  printf '{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$ans"
  printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","session_id":"s1","usage":{"input_tokens":1,"output_tokens":1}}\n' "$ans"
done
`

// liveEnv gives the test a short config dir (a unix socket path caps near 108 bytes) holding the fake claude.
func liveEnv(t *testing.T) { liveEnvScript(t, fakeLiveClaude) }

func liveEnvScript(t *testing.T, script string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
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
	rec := ClaimRecord("claude", Request{Timeout: idle}, time.Now())
	rec.Live, rec.PID = true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	done, id := make(chan error, 1), rec.ID
	go func() { done <- ServeLive(ctx, &rec, nil) }()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(LiveSocket(id)); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
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
	time.Sleep(500 * time.Millisecond)
	if _, err := SendLive(context.Background(), id, "second", Discard); err != ErrLiveBusy {
		t.Errorf("concurrent send = %v, want ErrLiveBusy", err)
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
		rec := ClaimRecord("claude", Request{}, time.Now())
		rec.Live, rec.PID = true, os.Getpid()
		_ = rec.Save()
	}
	if err := CheckLiveCap(""); err == nil {
		t.Fatal("cap must refuse a fifth session")
	}
	dead := ClaimRecord("claude", Request{}, time.Now())
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
	id, wait := startHolder(t, context.Background(), 5*time.Second)
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
