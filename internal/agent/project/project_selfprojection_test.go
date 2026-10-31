package project

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A vendor's own agent (agents/<v>.md, e.g. agents/codex.md declaring `name: codex`) is never
// projected into that vendor's own CLI home/dist (nesting hazard): syncVendor's own-name skip must
// also hold for the vendor's self-entry, same as any other agent.
func TestSyncSkipsVendorsOwnAgentEverywhere(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	writeSource(t, claude, "agents", "codex", "codex") // agents/codex.md declaring name: codex

	var out bytes.Buffer
	if code := Sync(Options{Agent: "codex", Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	dist := filepath.Join(Dir(claude, "codex"), "codex.toml")
	if _, err := os.Stat(dist); !os.IsNotExist(err) {
		t.Errorf("codex's own agent was projected into its own dist: %s", dist)
	}
	homeFile := filepath.Join(home, ".codex", "agents", "codex.toml")
	if _, err := os.Stat(homeFile); !os.IsNotExist(err) {
		t.Errorf("codex's own agent was projected into its own home: %s", homeFile)
	}
	// The other agent still projects normally; the skip is scoped to the self-entry only.
	if _, err := os.Stat(filepath.Join(Dir(claude, "codex"), "demo.toml")); err != nil {
		t.Errorf("unrelated agent not projected: %v", err)
	}
}

// Proves teeth: a stale self-projection left over from before the skip existed (or from a manual
// drop-in) is pruned on the next sync, in both dist and home, exactly like any other orphan.
func TestSyncPrunesExistingSelfProjection(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "codex", "codex")

	dist := filepath.Join(Dir(claude, "codex"), "codex.toml")
	homeFile := filepath.Join(home, ".codex", "agents", "codex.toml")
	stale := codexBannerPrefix + "codex.toml; do not edit by hand. -->\n\nstale self-projection\n"
	projectWriteFile(t, dist, stale)
	projectWriteFile(t, homeFile, stale)

	var out bytes.Buffer
	if code := Sync(Options{Agent: "codex", Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	if _, err := os.Lstat(dist); !os.IsNotExist(err) {
		t.Errorf("stale self-projection survived in dist: %s", dist)
	}
	if _, err := os.Lstat(homeFile); !os.IsNotExist(err) {
		t.Errorf("stale self-projection survived in home: %s", homeFile)
	}
	if code := Sync(Options{Agent: "codex", Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check after prune = %d, want 0", code)
	}
}
