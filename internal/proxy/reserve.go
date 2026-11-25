package proxy

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxyteam"
)

// scopeStartWait bounds how long a spawned scoped proxy has to publish its scope file and answer /healthz.
const scopeStartWait = 5 * time.Second

// EnsureTeam returns the live scoped proxy for a detached team's slot, starting or replacing it as needed.
func EnsureTeam(ctx context.Context, t proxyteam.Team, dispatchID string) (Scope, error) {
	if t.Host != proxyteam.HostDetached {
		return Scope{}, fmt.Errorf("proxy team %q: host %q is not detached", t.Name, t.Host)
	}
	id := slotID(dispatchID, t.Reserved)
	if err := checkScopeParts(t.Name, id); err != nil {
		return Scope{}, err
	}
	if sc, ok := liveScope(ctx, t.Name, id); ok {
		return sc, nil // a healthy slot serves as it is, even on an older binary: install and the status line replace it
	}
	var out Scope
	err := paths.WithFileLock(scopeFile(t.Name, id)+".spawn", func() error {
		sc, err := startSlot(ctx, t.Name, id)
		out = sc
		return err
	})
	return out, err
}

// slotID is the slot fnv32(dispatchID) % max(reserved, 1).
func slotID(dispatchID string, reserved int) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(dispatchID))
	return strconv.Itoa(int(h.Sum32() % uint32(max(reserved, 1))))
}

// liveScope returns the slot's record when its process is alive and answers /healthz as that pid.
func liveScope(ctx context.Context, team, id string) (Scope, bool) {
	sc, err := ReadScope(team, id)
	if err != nil || sc.Host != HostDetached || !scopeLive(sc) {
		return Scope{}, false
	}
	h, ok := Holder(ctx, sc.Port)
	if !ok || h.PID != sc.PID {
		return Scope{}, false
	}
	return sc, true
}

// SelfExeHash is the hash a scoped proxy records in its Scope; "" when the binary cannot be read.
func SelfExeHash() string { return selfHash() }

// ScopeStale is true when sc runs another binary than this one (an older record without a hash always does).
func ScopeStale(sc Scope) bool {
	self := selfHash()
	return self != "" && sc.Exe != self
}

// startSlot runs under the slot lock: it re-checks, replaces an unhealthy proxy, then spawns and waits for a live one.
func startSlot(ctx context.Context, team, id string) (Scope, error) {
	sc, ok := liveScope(ctx, team, id)
	if !ok {
		sc, ok = liveScope(ctx, team, id) // one more probe: a busy proxy may miss a single 1s one
	}
	if ok {
		return sc, nil // healthy: use it as it is, whatever binary it runs
	}
	if held() {
		return Scope{}, heldErr()
	}
	err := retireSlot(ctx, team, id)
	if !errors.Is(err, errSlotBusy) {
		if err != nil {
			return Scope{}, err
		}
		return spawnSlot(ctx, team, id)
	}
	if cur, rerr := ReadScope(team, id); rerr == nil && scopeLive(cur) {
		return cur, nil // it answered busy a moment ago: it stays the endpoint
	}
	return spawnSlot(ctx, team, id) // it exited since: start a fresh one
}

// spawnSlot starts the slot's proxy and waits for it, honoring a --stop hold that lands before or during the start.
func spawnSlot(ctx context.Context, team, id string) (Scope, error) {
	if err := ctx.Err(); err != nil {
		return Scope{}, err
	}
	if held() { // a --stop may have landed during the retire
		return Scope{}, heldErr()
	}
	kill, err := spawnScope(team, id)
	if err != nil {
		return Scope{}, err
	}
	sc, err := awaitScope(ctx, team, id)
	if err != nil {
		kill()
		return sc, err
	}
	if held() { // a --stop landed while it started: it would outlive the hold
		return Scope{}, errors.Join(heldErr(), stopScope(sc))
	}
	return sc, nil
}

func held() bool {
	_, err := os.Stat(heldPath())
	return err == nil
}

func heldErr() error {
	return errors.New("proxy: held down by `gateway proxy --stop`; `gateway proxy --start` lifts it")
}

// errSlotBusy means the slot's proxy answered the drain request busy, so it is not retired.
var errSlotBusy = errors.New("proxy: slot is busy")

// retireSlot stops a wedged (alive, not answering) proxy; an asker's scope is never signalled.
func retireSlot(ctx context.Context, team, id string) error {
	rec, err := ReadScope(team, id)
	if err != nil || !scopeLive(rec) {
		return nil
	}
	if rec.Host != HostDetached {
		return fmt.Errorf("proxy %s@%s is held by a process that is not a detached proxy", team, id)
	}
	idle, answered := drainIfIdle(ctx, rec)
	if err := ctx.Err(); err != nil && !answered { // an unanswered, cancelled probe proves nothing: never signal on it
		return err
	}
	if answered && !idle {
		return errSlotBusy
	}
	return stopScope(rec) // a proxy that may be draining is no endpoint: the caller takes its next team
}

// killGrace is how long a SIGKILLed scope gets to vanish.
const killGrace = 5 * time.Second

// scopeGone polls up to d for sc's process to be gone, by identity so a reused pid does not count as alive.
func scopeGone(sc Scope, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(100 * time.Millisecond) {
		if !scopeLive(sc) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// stopScope SIGTERMs sc while its start-time identity still holds, escalates to SIGKILL, and waits for it to exit.
func stopScope(sc Scope) error {
	if !scopeLive(sc) {
		return nil
	}
	if err := terminate(sc.PID); err != nil {
		if !scopeLive(sc) {
			return nil // it exited between the check and the signal
		}
		return err
	}
	if scopeGone(sc, ShutdownDrain+5*time.Second) {
		return nil
	}
	if !scopeLive(sc) { // the pid may be reused since the last check
		return nil
	}
	_ = kill(sc.PID)
	if scopeGone(sc, killGrace) {
		return nil
	}
	return fmt.Errorf("proxy %s@%s pid %d signalled but still draining", sc.Team, sc.ID, sc.PID)
}

func awaitScope(ctx context.Context, team, id string) (Scope, error) {
	for deadline := time.Now().Add(scopeStartWait); time.Now().Before(deadline); {
		if sc, ok := liveScope(ctx, team, id); ok {
			return sc, nil
		}
		select {
		case <-ctx.Done():
			if sc, ok := lastProbe(ctx, team, id); ok { // it came up as the caller gave up: keep it
				return sc, nil
			}
			return Scope{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if sc, ok := lastProbe(ctx, team, id); ok { // it may have turned healthy as the deadline passed
		return sc, nil
	}
	return Scope{}, fmt.Errorf("proxy %s@%s did not become healthy within %s (see %s)", team, id, scopeStartWait, logFile())
}

// lastProbe is awaitScope's final probe, which a cancelled ctx must not fail before the caller kills the child.
func lastProbe(ctx context.Context, team, id string) (Scope, bool) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	sc, ok := liveScope(pctx, team, id)
	return sc, ok
}

var spawnScope = spawnScopeProcess

// spawnScopeProcess starts the detached proxy and returns a func that kills it, for a start that never turns healthy.
func spawnScopeProcess(team, id string) (func(), error) {
	exe, err := paths.SelfBinary()
	if err != nil {
		return nil, err
	}
	if proc.IsSelfUnderTest(exe) {
		return nil, ErrTestBinary
	}
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(logFile()); err == nil && fi.Size() > maxLogBytes {
		flags |= os.O_TRUNC
	}
	lf, err := os.OpenFile(logFile(), flags, 0o600)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "gateway", "proxy", "--serve", "--scope", team+"@"+id) //nolint:noctx // outlives its caller
	cmd.Env = proc.Drop(os.Environ(), nil, []string{egress.EnvHost})
	cmd.Dir, cmd.Stdout, cmd.Stderr = paths.StateDir(), lf, lf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }() // reaps the child, so a long-lived caller sees its exit instead of a zombie
	return func() { _ = cmd.Process.Kill() }, nil
}
