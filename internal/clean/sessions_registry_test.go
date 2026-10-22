package clean

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedRegistryEntry lays down a sessions/<pid>.json + sessions/<pid>.<hex>.key pair (or any name,
// for the unparsable case) at the given mtime and returns both paths.
func seedRegistryEntry(t *testing.T, dir, name string, mtime time.Time) (jsonPath, keyPath string) {
	t.Helper()
	jsonPath = filepath.Join(dir, "sessions", name+".json")
	keyPath = filepath.Join(dir, "sessions", name+".deadbeef.key")
	for _, p := range []string{jsonPath, keyPath} {
		write(t, p, "x")
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return jsonPath, keyPath
}

// TestSessionsRegistryDeadPidOldEntryRemoved: a dead pid's registry pair, aged past --older-than,
// is removed under --target sessions --yes.
func TestSessionsRegistryDeadPidOldEntryRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, jsonPath) || exists(t, keyPath) {
		t.Error("dead pid's old registry entry survived --target sessions --yes")
	}
}

// TestSessionsRegistryAlivePidKept: an alive pid's registry pair is kept even though it is older
// than --older-than — liveness overrides age for this target's keep filter.
func TestSessionsRegistryAlivePidKept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return true }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("alive pid's registry entry was removed")
	}

	// Prove the stub has teeth: invert it and confirm the same fixture now gets removed.
	pidAlive = func(int) bool { return false }
	out.Reset()
	errw.Reset()
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, jsonPath) || exists(t, keyPath) {
		t.Error("inverted stub (dead) should have removed the entry — pidAlive stub has no effect")
	}
}

// TestSessionsRegistryDeadPidRecentEntryKept: a dead pid's registry pair newer than --older-than
// survives — the age gate still applies on top of the liveness filter.
func TestSessionsRegistryDeadPidRecentEntryKept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", time.Now())

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("dead pid's recent registry entry was removed despite being inside --older-than")
	}
}

// TestSessionsRegistryUnparsableNameKept: a registry-dir entry whose name does not start with a
// pid (unparsable) is kept, even when old and even though pidAlive is stubbed dead.
func TestSessionsRegistryUnparsableNameKept(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "notapid", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("unparsable registry entry name was removed; an unparsable pid prefix must be kept")
	}
}

// TestSessionsRegistryUntouchedWithoutTargetFlag: a bare `clean` (no --target sessions) never
// looks at the registry at all, dead pid or not.
func TestSessionsRegistryUntouchedWithoutTargetFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", old)

	var out, errw bytes.Buffer
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("bare Run = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("bare clean (no --target sessions) swept the session registry; sessions is opt-in")
	}
}

// TestSessionsRegressionTranscriptStillRemovedAlongsideRegistry: a transcript ages out under
// --target sessions exactly as before, unaffected by the new registry keep filter sitting next
// to it in the same target.
func TestSessionsRegressionTranscriptStillRemovedAlongsideRegistry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return true } // alive: registry entry must survive while transcript still goes
	t.Cleanup(func() { pidAlive = oldPidAlive })

	transcript, sessDir := seedAgedSession(t, dir)
	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, transcript) || exists(t, sessDir) {
		t.Error("transcript/session dir survived --target sessions --yes — registry filter regressed transcript pruning")
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("alive pid's registry entry was removed alongside the transcript")
	}
}
