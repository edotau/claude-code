package project

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
	projection := filepath.Join(home, ".codex", "agents", "demo-agent.toml")

	if code := Sync(Options{Check: true, DryRun: true, Out: io.Discard}); code == 0 {
		t.Error("--check on a missing projection = 0, want non-zero")
	}
	if _, err := os.Stat(projection); err == nil {
		t.Error("--check must not write")
	}
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("sync exit = %d, want 0", code)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check on a current projection = %d, want 0", code)
	}
	// Editing the source must surface as drift in the tracked copy AND the installed copy.
	projectWriteFile(t, sourcePath(claude, "agents", "demo-agent"), "---\nname: demo-agent\ndescription: changed.\n---\n\nnew body\n")
	var out bytes.Buffer
	if code := Sync(Options{Check: true, Out: &out}); code == 0 {
		t.Error("--check after a source edit = 0, want non-zero")
	}
	if !strings.Contains(out.String(), "DRY   ~/.claude/state/projections/codex/agents/demo-agent.toml") {
		t.Errorf("drift in the tracked dist copy not reported:\n%s", out.String())
	}
}

// Check implies DryRun inside Sync itself; a caller setting Check alone must never trigger a real write.
func TestSyncCheckAloneNeverWrites(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")

	if code := Sync(Options{Check: true, Out: io.Discard}); code == 0 {
		t.Error("--check on a missing projection = 0, want non-zero")
	}
	for _, p := range []string{filepath.Join(home, ".gemini"), filepath.Join(claude, "state", "projections")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("Check alone (DryRun unset) wrote %s", p)
		}
	}
}

// An agents dir that exists but holds no valid sources must not let prune wipe any vendor surface.
func TestSyncRefusesPruneOnEmptyDiscovery(t *testing.T) {
	home, claude := isolate(t)
	if err := os.MkdirAll(filepath.Join(claude, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, ".gemini", "skills", "real-skill")
	projectWriteFile(t, filepath.Join(legacy, "SKILL.md"), realSkillBannerPrefix+"real-skill/SKILL.md; do not edit by hand. -->\n\nbody\n")
	agentFile := filepath.Join(home, ".copilot", "agents", "ghost.md")
	projectWriteFile(t, agentFile, agentSkillBannerPrefix+"ghost.md; do not edit by hand. -->\n\nbody\n")

	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	for _, p := range []string{legacy, agentFile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("prune wiped %s on empty discovery: %v", p, err)
		}
	}
}

// Orphaned generated files are pruned from dist and every vendor home; hand-authored files at the same paths stay.
func TestSyncPrunesOrphansButKeepsHandAuthoredFiles(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "agents", "doomed", "doomed")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if err := os.Remove(sourcePath(claude, "agents", "doomed")); err != nil {
		t.Fatal(err)
	}
	hand := filepath.Join(home, ".codex", "agents", "mine.toml")
	projectWriteFile(t, hand, "name = \"mine\"\n")
	legacy := filepath.Join(home, ".gemini", "skills", "old-skill")
	projectWriteFile(t, filepath.Join(legacy, "SKILL.md"), realSkillBannerPrefix+"old-skill/SKILL.md; do not edit by hand. -->\n\nbody\n")

	var out bytes.Buffer
	if code := Sync(Options{Out: &out}); code != 0 {
		t.Fatalf("prune Sync exit = %d, want 0\n%s", code, out.String())
	}
	for _, vendor := range Names() {
		tg := targets[vendor]
		for _, p := range []string{filepath.Join(Dir(claude, vendor), "doomed"+tg.ext), filepath.Join(tg.home(home), "doomed"+tg.ext)} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("orphan survived prune: %s", p)
			}
		}
		if _, err := os.Stat(filepath.Join(tg.home(home), "keeper"+tg.ext)); err != nil {
			t.Errorf("%s: keeper pruned: %v", vendor, err)
		}
	}
	if _, err := os.Stat(hand); err != nil {
		t.Errorf("hand-authored codex agent pruned: %v", err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy ~/.gemini/skills projection survived: %v", err)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check after prune = %d, want 0", code)
	}
}

// A hand-authored file in a vendor home is never overwritten, even under a projected name.
func TestSyncKeepsHandAuthoredVendorFile(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "shadowed", "shadowed")
	hand := filepath.Join(home, ".gemini", "agents", "shadowed.md")
	projectWriteFile(t, hand, "---\nname: shadowed\ndescription: mine\n---\n\nhand-made\n")

	var out bytes.Buffer
	if code := Sync(Options{Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "KEEP  ~/.gemini/agents/shadowed.md (hand-authored") {
		t.Errorf("hand-authored vendor file not KEEP'd:\n%s", out.String())
	}
	assertFileContains(t, hand, "hand-made")
	assertFileContains(t, filepath.Join(Dir(claude, "gemini"), "shadowed.md"), agentSkillBannerPrefix)
}

// skills/<x> symlinks point into ~/.agents/skills already, so they are the user's and never re-linked.
func TestDiscoverSkillsSkipsSymlinkedDirs(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "skills", "real", "real")
	external := filepath.Join(claude, "..", ".agents", "skills", "linked")
	projectWriteFile(t, filepath.Join(external, "SKILL.md"), "---\nname: linked\n---\n\nbody\n")
	if err := os.Symlink(external, filepath.Join(claude, "skills", "linked")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(claude, "skills", "synced", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	skills, err := DiscoverSkills(filepath.Join(claude, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name() != "real" {
		names := make([]string, 0, len(skills))
		for _, s := range skills {
			names = append(names, s.Name())
		}
		t.Errorf("DiscoverSkills = %v, want [real]", names)
	}
}

// ~/.agents/skills: a real dir or a foreign symlink under a projected name is kept; a stale harness link is
// re-pointed; an orphaned harness link is pruned.
func TestSkillLinksRespectOwnership(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "skills", "mine-dir", "mine")
	writeSource(t, claude, "skills", "foreign-dir", "foreign")
	writeSource(t, claude, "skills", "moved-new", "moved")
	agentsSkills := AgentsSkillsDir(home)
	projectWriteFile(t, filepath.Join(agentsSkills, "mine", "SKILL.md"), "user's own copy\n")
	vendor := filepath.Join(home, "vendor", "foreign")
	projectWriteFile(t, filepath.Join(vendor, "SKILL.md"), "vendor copy\n")
	for link, target := range map[string]string{
		"foreign": vendor,
		"moved":   filepath.Join(claude, "skills", "moved-old"),
		"gone":    filepath.Join(claude, "skills", "gone"),
	} {
		if err := os.Symlink(target, filepath.Join(agentsSkills, link)); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
	}

	var out bytes.Buffer
	if code := Sync(Options{Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	if fi, err := os.Lstat(filepath.Join(agentsSkills, "mine")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("user's real skill dir was replaced")
	}
	if dest, _ := os.Readlink(filepath.Join(agentsSkills, "foreign")); dest != vendor {
		t.Errorf("foreign symlink re-pointed to %q", dest)
	}
	if dest, _ := os.Readlink(filepath.Join(agentsSkills, "moved")); dest != filepath.Join(claude, "skills", "moved-new") {
		t.Errorf("stale harness link not re-pointed: %q", dest)
	}
	if _, err := os.Lstat(filepath.Join(agentsSkills, "gone")); !os.IsNotExist(err) {
		t.Error("orphaned harness link not pruned")
	}
	assertFileContains(t, filepath.Join(agentsSkills, "mine", "SKILL.md"), "user's own copy")
}

// The gemini vendor pass links skills into agy's own GeminiSkillsDir, not just ~/.agents/skills.
func TestGeminiVendorSyncLinksOwnSkillsDir(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	writeSource(t, claude, "skills", "mine-dir", "mine")

	var out bytes.Buffer
	if code := Sync(Options{Agent: "gemini", Out: &out}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0\n%s", code, out.String())
	}
	geminiSkills := GeminiSkillsDir(home)
	if fi, err := os.Lstat(filepath.Join(geminiSkills, "mine")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("GeminiSkillsDir/mine not linked: %v", err)
	}
	dest, err := os.Readlink(filepath.Join(geminiSkills, "mine"))
	if err != nil || dest != filepath.Join(claude, "skills", "mine-dir") {
		t.Errorf("GeminiSkillsDir/mine -> %q (err %v), want %s", dest, err, filepath.Join(claude, "skills", "mine-dir"))
	}
}

// --vendor narrows the run: only that vendor's dist + home are touched, skills still link.
func TestSyncVendorFilter(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	if code := Sync(Options{Agent: "nope", Out: io.Discard}); code != 2 {
		t.Errorf("unknown vendor exit = %d, want 2", code)
	}
	if code := Sync(Options{Agent: "codex", Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "agents", "demo.toml")); err != nil {
		t.Errorf("codex projection missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("--vendor codex touched ~/.gemini")
	}
}
