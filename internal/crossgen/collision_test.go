package crossgen

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Two skill dirs sharing one `name:` would link the same ~/.agents/skills entry; the run must fail before writing.
func TestSyncRejectsDuplicateSkillNamesBeforeWriting(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "skills", "first", "duplicate")
	writeSource(t, claude, "skills", "second", "duplicate")

	if code := Sync(SyncOptions{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	for _, p := range []string{filepath.Join(claude, "docs", "projections"), filepath.Join(home, ".agents"), filepath.Join(home, ".codex")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after rejected collision: %v", p, err)
		}
	}
}

// An agent whose frontmatter name differs from its dir breaks memory routing and the roster invariant.
func TestDiscoverAgentsRejectsNameDirMismatch(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "real-dir", "other-name")

	if _, err := DiscoverAgents(filepath.Join(claude, "agents")); err == nil {
		t.Fatal("DiscoverAgents accepted name != dir")
	}
	if code := Sync(SyncOptions{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(claude, "docs", "projections")); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected roster: %v", err)
	}
}

func TestDuplicateValidationAllowsAgentAndSkillToShareName(t *testing.T) {
	agents := []Source{{dirName: "shared", frontmatter: map[string]any{"name": "shared"}}}
	skills := []Source{{dirName: "skill-dir", frontmatter: map[string]any{"name": "shared"}}}
	if err := validateUniqueNames("agent", agents); err != nil {
		t.Fatalf("agent validation: %v", err)
	}
	if err := validateUniqueNames("skill", skills); err != nil {
		t.Fatalf("skill validation: %v", err)
	}
}

// Agents and skills live on different surfaces now (vendor agent dirs vs ~/.agents/skills), so a shared name is
// not a collision: both project, and a NoPrune run leaves exactly what a pruning run leaves.
func TestSyncProjectsAgentsToEveryVendorAndLinksSkills(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "unique-agent", "unique-agent")
	writeSource(t, claude, "agents", "shared", "shared")
	writeSource(t, claude, "skills", "shared-skill", "shared")
	writeSource(t, claude, "skills", "real-only", "real-only")

	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	for _, vendor := range VendorNames() {
		tg := targets[vendor]
		for _, name := range []string{"unique-agent", "shared"} {
			dist := filepath.Join(ProjectionDir(claude, vendor), name+tg.ext)
			installed := filepath.Join(tg.home(home), name+tg.ext)
			a, err1 := os.ReadFile(dist)
			b, err2 := os.ReadFile(installed)
			if err1 != nil || err2 != nil || string(a) != string(b) {
				t.Errorf("%s/%s: projection and home copies differ or missing (%v, %v)", vendor, name, err1, err2)
			}
		}
	}
	for link, dir := range map[string]string{"shared": "shared-skill", "real-only": "real-only"} {
		dest, err := os.Readlink(filepath.Join(AgentsSkillsDir(home), link))
		if err != nil || dest != filepath.Join(claude, "skills", dir) {
			t.Errorf("~/.agents/skills/%s -> %q (%v), want %s", link, dest, err, filepath.Join(claude, "skills", dir))
		}
	}
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check right after a full sync = %d, want 0 (sync must be idempotent)", code)
	}
}
