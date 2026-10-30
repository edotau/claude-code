package clean

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/copilotreg"
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
// is removed under --target session-registry --yes.
func TestSessionsRegistryDeadPidOldEntryRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, jsonPath) || exists(t, keyPath) {
		t.Error("dead pid's old registry entry survived --target session-registry --yes")
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
	if code := Run([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("alive pid's registry entry was removed")
	}

	// Prove the stub has teeth: invert it and confirm the same fixture now gets removed.
	pidAlive = func(int) bool { return false }
	out.Reset()
	errw.Reset()
	if code := Run([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, jsonPath) || exists(t, keyPath) {
		t.Error("inverted stub (dead) should have removed the entry — pidAlive stub has no effect")
	}
}

// TestSessionsRegistryDeadPidRecentEntryRemoved: a dead pid's registry pair newer than --older-than
// is still removed — session-registry is ageless.
func TestSessionsRegistryDeadPidRecentEntryRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(int) bool { return false }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	jsonPath, keyPath := seedRegistryEntry(t, dir, "4242", time.Now())

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
	}
	if exists(t, jsonPath) || exists(t, keyPath) {
		t.Error("dead pid's recent registry entry survived; session-registry is ageless")
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
	if code := Run([]string{"--target", "session-registry", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target session-registry --yes = %d, want 0: %s", code, errw.String())
	}
	if !exists(t, jsonPath) || !exists(t, keyPath) {
		t.Error("unparsable registry entry name was removed; an unparsable pid prefix must be kept")
	}
}

// TestSessionsRegistrySplitFromSessionsTarget: bare clean prunes dead registry entries and keeps live
// ones; --target sessions (transcripts only) leaves the registry alone.
func TestSessionsRegistrySplitFromSessionsTarget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	oldPidAlive := pidAlive
	pidAlive = func(pid int) bool { return pid == 1111 }
	t.Cleanup(func() { pidAlive = oldPidAlive })

	old := time.Now().Add(-40 * 24 * time.Hour)
	deadJSON, deadKey := seedRegistryEntry(t, dir, "4242", old)
	liveJSON, liveKey := seedRegistryEntry(t, dir, "1111", old)

	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "sessions", "--yes"}, &out, &errw); code != 0 {
		t.Fatalf("Run --target sessions = %d: %s", code, errw.String())
	}
	if !exists(t, deadJSON) || !exists(t, deadKey) {
		t.Error("--target sessions removed registry files; they belong to session-registry")
	}
	if code := Run(nil, &out, &errw); code != 0 {
		t.Fatalf("bare Run = %d: %s", code, errw.String())
	}
	if exists(t, deadJSON) || exists(t, deadKey) {
		t.Error("bare clean left a dead pid's registry entry")
	}
	if !exists(t, liveJSON) || !exists(t, liveKey) {
		t.Error("bare clean removed a live pid's registry entry")
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

func TestCleanCopilotRegistryTargetKeepsLivePid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux runtime dir")
	}
	base := "/run/user/" + strconv.Itoa(os.Getuid())
	if fi, err := os.Stat(base); err != nil || !fi.IsDir() {
		t.Skip("no tmpfs /run/user/<uid>")
	}
	t.Setenv("XDG_RUNTIME_DIR", base)
	live, err := copilotreg.WriteRegistry(os.Getpid(), []byte("{}"))
	if err != nil {
		t.Skipf("runtime dir unusable here: %v", err)
	}
	t.Cleanup(func() { os.Remove(live) })
	dead := copilotreg.RegistryFile(filepath.Dir(live), 1<<22+12346)
	write(t, dead, "{}")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var out, errw bytes.Buffer
	if code := Run([]string{"--target", "copilot-registry"}, &out, &errw); code != 0 {
		t.Fatalf("Run = %d: %s", code, errw.String())
	}
	if exists(t, dead) || !exists(t, live) {
		t.Fatalf("dead kept=%v live kept=%v\n%s", exists(t, dead), exists(t, live), out.String())
	}
}
