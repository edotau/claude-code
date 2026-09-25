package crossgen

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --check must fail on a stale projection and pass once it is current, without ever writing.
func TestSyncCheckGatesStaleProjection(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	projection := filepath.Join(home, ".gemini", "skills", "demo-agent", "SKILL.md")

	if code := Sync(SyncOptions{Check: true, DryRun: true, Out: io.Discard}); code == 0 {
		t.Error("--check on a missing projection = 0, want non-zero")
	}
	if _, err := os.Stat(projection); err == nil {
		t.Error("--check must not write")
	}
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("sync exit = %d, want 0", code)
	}
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check on a current projection = %d, want 0", code)
	}
}

// Check implies DryRun inside Sync itself; a caller setting Check alone must never trigger a real write.
func TestSyncCheckAloneNeverWrites(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")

	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code == 0 {
		t.Error("--check on a missing projection = 0, want non-zero")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "skills", "demo-agent")); err == nil {
		t.Error("Check alone (DryRun unset) must not write")
	}
}

// An agents dir that exists but holds no valid sources must not let prune wipe ~/.gemini/skills.
func TestSyncRefusesPruneOnEmptyDiscovery(t *testing.T) {
	home, claude := isolate(t)
	if err := os.MkdirAll(filepath.Join(claude, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, ".gemini", "skills", "real-skill")
	writeFile(t, filepath.Join(victim, "SKILL.md"), realSkillBannerPrefix+"real-skill/SKILL.md; do not edit by hand. -->\n\nbody\n")

	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("prune wiped the projection on empty discovery: %v", err)
	}
}

// A projected skill's body cites its own scripts/ and references/, so the projection must carry them —
// linked, idempotent, and removed with the projection once the source is gone.
func TestRealSkillProjectionCarriesAssetDirs(t *testing.T) {
	home, claude := isolate(t)
	if err := os.MkdirAll(filepath.Join(claude, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(claude, "skills", "demo-skill")
	writeFile(t, filepath.Join(src, "SKILL.md"), "---\nname: demo-skill\ndescription: cites scripts/checker.py\n---\n\nRun `scripts/checker.py`.\n")
	writeFile(t, filepath.Join(src, "scripts", "checker.py"), "print('hi')\n")
	writeFile(t, filepath.Join(src, "references", "detectors.md"), "# detectors\n")
	writeFile(t, filepath.Join(src, "extra.md"), "# extra\n")

	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	proj := filepath.Join(home, ".gemini", "skills", "demo-skill")
	for _, rel := range []string{"scripts/checker.py", "references/detectors.md", "extra.md"} {
		if _, err := os.Stat(filepath.Join(proj, rel)); err != nil {
			t.Errorf("projection missing %s: %v", rel, err)
		}
	}
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code != 0 {
		t.Fatalf("--check after a full sync = %d, want 0 (asset links must be idempotent)", code)
	}

	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	writeSource(t, claude, "skills", "keeper", "keeper")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("prune Sync exit = %d, want 0", code)
	}
	if _, err := os.Lstat(proj); !os.IsNotExist(err) {
		t.Errorf("orphaned projection survived prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claude, "skills", "keeper", "SKILL.md")); err != nil {
		t.Errorf("prune must never reach through a link into the source tree: %v", err)
	}
}

// skills/<x> symlinks point into ~/.agents/skills, which Gemini CLI already reads: projecting them duplicates names.
func TestDiscoverSkillsSkipsSymlinkedDirs(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "skills", "real", "real")
	external := filepath.Join(claude, "..", ".agents", "skills", "linked")
	writeFile(t, filepath.Join(external, "SKILL.md"), "---\nname: linked\n---\n\nbody\n")
	if err := os.Symlink(external, filepath.Join(claude, "skills", "linked")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(claude, "skills", "synced", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	skills := DiscoverSkills(filepath.Join(claude, "skills"))
	if len(skills) != 1 || skills[0].Name() != "real" {
		names := make([]string, 0, len(skills))
		for _, s := range skills {
			names = append(names, s.Name())
		}
		t.Errorf("DiscoverSkills = %v, want [real]", names)
	}
}

// A symlink entry in ~/.gemini/skills is never written through or pruned, even when it shadows a projected name.
func TestSyncKeepsSymlinkEntriesInGeminiSkills(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "skills", "shadowed", "shadowed")
	root := filepath.Join(home, ".gemini", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(home, "vendor-skills")
	writeFile(t, filepath.Join(external, "shadowed", "SKILL.md"), "hand-made\n")
	writeFile(t, filepath.Join(external, "foreign", "SKILL.md"), realSkillBannerPrefix+"foreign/SKILL.md -->\n")
	for _, n := range []string{"shadowed", "foreign"} {
		if err := os.Symlink(filepath.Join(external, n), filepath.Join(root, n)); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
	}

	var out bytes.Buffer
	if code := Sync(SyncOptions{Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "KEEP  ~/.gemini/skills/shadowed (hand-authored symlink)") {
		t.Errorf("shadowing symlink not KEEP'd:\n%s", out.String())
	}
	for _, n := range []string{"shadowed", "foreign"} {
		info, err := os.Lstat(filepath.Join(root, n))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("symlink entry %q did not survive as a symlink: %v", n, err)
		}
	}
	assertFileContains(t, filepath.Join(external, "shadowed", "SKILL.md"), "hand-made")
}
