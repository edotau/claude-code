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
	if sc, inflight, ok := liveScope(ctx, t.Name, id); ok && (!scopeStale(sc) || inflight > 0) {
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

// scopeStale is true when the installed binary was written after the scoped proxy started.
func scopeStale(sc Scope) bool {
	exe, err := paths.SelfBinary()
	if err != nil {
		return false
	}
	fi, err := os.Stat(exe)
	return err == nil && fi.ModTime().After(sc.Started)
}

// startSlot runs under the slot lock: it re-checks, replaces an idle stale proxy, then spawns and waits for a live one.
func startSlot(ctx context.Context, team, id string) (Scope, error) {
	if sc, inflight, ok := liveScope(ctx, team, id); ok {
		if !scopeStale(sc) || inflight > 0 || !stopScope(sc) {
			return sc, nil // fresh, busy or unstoppable: use it as it is
		}
	}
	if err := spawnScope(team, id); err != nil {
		return Scope{}, err
	}
	return awaitScope(ctx, team, id)
}

// stopScope SIGTERMs sc and reports whether it exited, so the replacement can take its scope file.
func stopScope(sc Scope) bool { return terminate(sc.PID) == nil && awaitExit(sc.PID) }

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
	return Scope{}, fmt.Errorf("proxy %s@%s did not become healthy within %s (see %s)", team, id, scopeStartWait, logFile())
}

var spawnScope = spawnScopeProcess

func spawnScopeProcess(team, id string) error {
	exe, err := paths.SelfBinary()
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(exe) {
		return ErrTestBinary
	}
	lf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "gateway", "proxy", "--serve", "--scope", team+"@"+id) //nolint:noctx // outlives its caller
	cmd.Env = proc.Drop(os.Environ(), nil, []string{egress.EnvHost})
	cmd.Dir, cmd.Stdout, cmd.Stderr = paths.StateDir(), lf, lf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reaps the child, so a long-lived caller sees its exit instead of a zombie
	return nil
}
