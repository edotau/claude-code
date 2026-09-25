package crossgen

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Two skill dirs sharing one `name:` would write the same output dir; the run must fail before writing.
func TestSyncRejectsDuplicateSkillNamesBeforeWriting(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "skills", "first", "duplicate")
	writeSource(t, claude, "skills", "second", "duplicate")

	if code := Sync(SyncOptions{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "skills")); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected collision: %v", err)
	}
}

// An agent whose frontmatter name differs from its dir breaks memory routing and the roster invariant.
func TestDiscoverAgentsRejectsNameDirMismatch(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "real-dir", "other-name")

	if _, err := DiscoverAgents(filepath.Join(claude, "agents")); err == nil {
		t.Fatal("DiscoverAgents accepted name != dir")
	}
	if code := Sync(SyncOptions{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "skills")); !os.IsNotExist(err) {
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

// Prune must never delete what the same run just wrote: write side and keep-set once drifted and a live run
// wrote every skill then pruned them all as orphans. A NoPrune run vs a pruning run asserts the invariant.
func TestSyncPruneNeverDeletesWhatTheSameRunWrote(t *testing.T) {
	written := syncedSkillDirs(t, true)
	surviving := syncedSkillDirs(t, false)
	if strings.Join(written, ",") != strings.Join(surviving, ",") {
		t.Errorf("prune removed same-run output:\n  written:   %v\n  surviving: %v", written, surviving)
	}
	if len(written) == 0 {
		t.Fatal("no projections written — the comparison above would pass vacuously")
	}
}

// syncedSkillDirs runs one Sync into a fresh HOME and returns the sorted entries left under ~/.gemini/skills.
func syncedSkillDirs(t *testing.T, noPrune bool) []string {
	t.Helper()
	home, claude := isolate(t)
	writeRoster(t, claude)
	if code := Sync(SyncOptions{NoPrune: noPrune, Out: io.Discard}); code != 0 {
		t.Fatalf("Sync(noPrune=%v) exit = %d, want 0", noPrune, code)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".gemini", "skills"))
	if err != nil {
		t.Fatalf("read projection root: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// writeRoster: two agents (one whose name a real skill also claims) and two real skills.
func writeRoster(t *testing.T, claude string) {
	t.Helper()
	writeSource(t, claude, "agents", "unique-agent", "unique-agent")
	writeSource(t, claude, "agents", "shared", "shared")
	writeSource(t, claude, "skills", "shared-skill", "shared")
	writeSource(t, claude, "skills", "real-only", "real-only")
}

// Agents and real skills both project; on a name collision the real skill owns the output.
func TestSyncProjectsAgentsAndRealSkillsWithSkillWinningCollisions(t *testing.T) {
	home, claude := isolate(t)
	writeRoster(t, claude)

	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	root := filepath.Join(home, ".gemini", "skills")
	assertFileContains(t, filepath.Join(root, "unique-agent", "SKILL.md"), agentSkillBannerPrefix+"unique-agent/agent.md")
	assertFileContains(t, filepath.Join(root, "shared", "SKILL.md"), realSkillBannerPrefix+"shared-skill/SKILL.md")
	assertFileContains(t, filepath.Join(root, "real-only", "SKILL.md"), realSkillBannerPrefix)
}
