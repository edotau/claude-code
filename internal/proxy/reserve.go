package proxy

import (
	"context"
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
	if sc, inflight, ok := liveScope(ctx, t.Name, id); ok && (!ScopeStale(sc) || inflight > 0) {
		return sc, nil
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
func liveScope(ctx context.Context, team, id string) (Scope, int64, bool) {
	sc, err := ReadScope(team, id)
	if err != nil || !scopeLive(sc) {
		return Scope{}, 0, false
	}
	h, ok := Holder(ctx, sc.Port)
	if !ok || h.PID != sc.PID {
		return Scope{}, 0, false
	}
	return sc, h.Inflight, true
}

// SelfExeHash is the hash a scoped proxy records in its Scope; "" when the binary cannot be read.
func SelfExeHash() string { return selfHash() }

// ScopeStale is true when sc runs another binary than this one (an older record without a hash always does).
func ScopeStale(sc Scope) bool {
	self := selfHash()
	return self != "" && sc.Exe != self
}

// startSlot runs under the slot lock: it re-checks, replaces an idle stale proxy, then spawns and waits for a live one.
func startSlot(ctx context.Context, team, id string) (Scope, error) {
	sc, inflight, ok := liveScope(ctx, team, id)
	if ok && (!ScopeStale(sc) || inflight > 0) {
		return sc, nil // fresh or busy: use it as it is
	}
	if err := retireSlot(team, id); err != nil {
		return Scope{}, err
	}
	if err := ctx.Err(); err != nil {
		return Scope{}, err
	}
	kill, err := spawnScope(team, id)
	if err != nil {
		return Scope{}, err
	}
	sc, err = awaitScope(ctx, team, id)
	if err != nil {
		kill()
	}
	return sc, err
}

// retireSlot stops a stale idle or wedged (alive, not answering) proxy; an asker's scope is never signalled.
func retireSlot(team, id string) error {
	rec, err := ReadScope(team, id)
	if err != nil || !scopeLive(rec) {
		return nil
	}
	if rec.Host != HostDetached {
		return fmt.Errorf("proxy %s@%s is held by a process that is not a detached proxy", team, id)
	}
	return stopScope(rec) // a proxy that may be draining is no endpoint: the caller takes its next team
}

// stopScope SIGTERMs sc while its start-time identity still holds, and waits for it to exit.
func stopScope(sc Scope) error {
	if !scopeLive(sc) {
		return nil
	}
	if err := terminate(sc.PID); err != nil {
		return err
	}
	if !awaitExit(sc.PID) {
		return fmt.Errorf("proxy %s@%s pid %d signalled but still draining", sc.Team, sc.ID, sc.PID)
	}
	return nil
}

func awaitScope(ctx context.Context, team, id string) (Scope, error) {
	for deadline := time.Now().Add(scopeStartWait); time.Now().Before(deadline); {
		if sc, _, ok := liveScope(ctx, team, id); ok {
			return sc, nil
		}
		select {
		case <-ctx.Done():
			return Scope{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if sc, _, ok := liveScope(ctx, team, id); ok { // it may have turned healthy as the deadline passed
		return sc, nil
	}
	return Scope{}, fmt.Errorf("proxy %s@%s did not become healthy within %s (see %s)", team, id, scopeStartWait, logFile())
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
