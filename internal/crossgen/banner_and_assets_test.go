package crossgen

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// hasGeneratedBanner (via isGeneratedSkillDir) only recognises the banner as the FIRST non-empty body line;
// a hand-authored file that merely quotes the banner text further down must read as hand-authored (KEEP).
func TestGeneratedBannerMustBeFirstBodyLine(t *testing.T) {
	dir := t.TempDir()

	hand := filepath.Join(dir, "x")
	writeFile(t, filepath.Join(hand, "SKILL.md"), "---\nname: x\ndescription: d\n---\n\n"+
		"Some hand-written prose.\n\nIt later quotes: "+agentSkillBannerPrefix+"agents/x.md; do not edit by hand. -->\n")
	if isGeneratedSkillDir(hand) {
		t.Error("banner quoted later in the body must not be recognised as generated (would KEEP)")
	}

	gen := filepath.Join(dir, "y")
	writeFile(t, filepath.Join(gen, "SKILL.md"), "---\nname: y\ndescription: d\n---\n\n"+
		agentSkillBannerPrefix+"agents/y.md; do not edit by hand. -->\n\nBody.\n")
	if !isGeneratedSkillDir(gen) {
		t.Error("banner as the first body line must be recognised as generated")
	}
}

// TestPruneReasonSupersededBySkill: an agent and a real skill share a name; a stale dir under that name,
// left by a prior agent-only projection, should be reported as pruned because a skill now supersedes it.
// A stale agent projection under a name a real skill now owns is rewritten by the skill, never pruned.
func TestSkillRewritesSupersededAgentProjection(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "shared", "SKILL.md")
	writeFile(t, stale, agentSkillBannerPrefix+"shared/agent.md; do not edit by hand. -->\n\n# shared\n")

	agents := []Source{{dirName: "shared", frontmatter: map[string]any{"name": "shared"}}}
	skills := []Source{{dirName: "shared-skill", frontmatter: map[string]any{"name": "shared"}}}

	var out bytes.Buffer
	if n := pruneOrphans(&out, agents, skills, projectedNames(agents, skills), root, false); n != 0 {
		t.Fatalf("pruned %d, want 0: %s", n, out.String())
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale projection must survive prune for the skill to overwrite it: %v", err)
	}
}

// linkSkillAssets must symlink real content dirs/files but skip SKILL.md itself, dotfiles, and __pycache__.
func TestLinkSkillAssetsSkipsDotfilesAndPycache(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "SKILL.md"), "skill\n")
	writeFile(t, filepath.Join(src, "scripts", "run.py"), "print(1)\n")
	writeFile(t, filepath.Join(src, ".hidden"), "secret\n")
	writeFile(t, filepath.Join(src, "__pycache__", "x.pyc"), "bin\n")

	out := t.TempDir()
	var buf bytes.Buffer
	n := linkSkillAssets(&buf, src, out, false)
	if n != 1 {
		t.Errorf("linkSkillAssets returned %d links, want 1: %s", n, buf.String())
	}
	if info, err := os.Lstat(filepath.Join(out, "scripts")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("scripts/ not linked: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out, ".hidden")); err == nil {
		t.Error(".hidden must not be linked")
	}
	if _, err := os.Lstat(filepath.Join(out, "__pycache__")); err == nil {
		t.Error("__pycache__ must not be linked")
	}
	if _, err := os.Lstat(filepath.Join(out, "SKILL.md")); err == nil {
		t.Error("SKILL.md must not be linked")
	}
}
