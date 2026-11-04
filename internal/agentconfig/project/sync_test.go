package project

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Two skill dirs sharing one `name:` would link the same ~/.agents/skills entry; the run must fail before writing.
func TestSyncRejectsDuplicateSkillNamesBeforeWriting(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "skills", "first", "duplicate")
	writeSource(t, claude, "skills", "second", "duplicate")

	if code := Sync(Options{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	for _, p := range []string{filepath.Join(claude, "state", "projections"), filepath.Join(home, ".agents"), filepath.Join(home, ".codex")} {
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
	if code := Sync(Options{NoPrune: true, Out: io.Discard}); code != 1 {
		t.Fatalf("Sync exit = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(claude, "state", "projections")); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejected roster: %v", err)
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

	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	assertAgentsProjectedEverywhere(t, home, claude, "unique-agent", "shared")
	for link, dir := range map[string]string{"shared": "shared-skill", "real-only": "real-only"} {
		dest, err := os.Readlink(filepath.Join(AgentsSkillsDir(home), link))
		if err != nil || dest != filepath.Join(claude, "skills", dir) {
			t.Errorf("~/.agents/skills/%s -> %q (%v), want %s", link, dest, err, filepath.Join(claude, "skills", dir))
		}
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check right after a full sync = %d, want 0 (sync must be idempotent)", code)
	}
}

// assertAgentsProjectedEverywhere requires every named agent to exist, identical, in each vendor's dist and home.
func assertAgentsProjectedEverywhere(t *testing.T, home, claude string, agents ...string) {
	t.Helper()
	for _, vendor := range Names() {
		tg := targets[vendor]
		for _, name := range agents {
			dist := filepath.Join(Dir(claude, vendor), name+tg.ext)
			installed := filepath.Join(tg.home(home), name+tg.ext)
			a, err1 := os.ReadFile(dist)
			b, err2 := os.ReadFile(installed)
			if err1 != nil || err2 != nil || string(a) != string(b) {
				t.Errorf("%s/%s: projection and home copies differ or missing (%v, %v)", vendor, name, err1, err2)
			}
		}
	}
}

// newDocsHarness builds a minimal harness (CLAUDE.md + one agent) under an isolated CLAUDE_CONFIG_DIR.
func newDocsHarness(t *testing.T) (home, claude string) {
	t.Helper()
	home, claude = isolate(t)
	projectWriteFile(t, filepath.Join(claude, "CLAUDE.md"), "# CLAUDE.md\n\nBody.\n\n\n")
	projectWriteFile(t, filepath.Join(claude, "agents", "demo-agent.md"),
		"---\nname: demo-agent\ndescription: demo agent for the docs test. More words.\nmodel: sonnet\n---\n\nBody.\n")
	return home, claude
}

// The instruction render lands in each vendor's GLOBAL file (what its CLI reads from $HOME) plus the local projection
// copy, byte-identical, and never at the harness root — Claude Code would load a root AGENTS.md twice.
func TestSyncRendersVendorInstructionFilesNotRootCopies(t *testing.T) {
	home, claude := newDocsHarness(t)
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	want := DocsBanner + "\n\n" + docsPathsNote + "\n\n# CLAUDE.md\n\nBody.\n\n## Agents\n\n- **demo-agent** (`sonnet`) — demo agent for the docs test.\n"
	for vendor, rel := range instructionFiles {
		dist := filepath.Join(Dir(claude, vendor), "..", filepath.Base(rel))
		for _, p := range []string{filepath.Join(home, rel), dist} {
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if string(got) != want {
				t.Errorf("%s mismatch\n got: %q\nwant: %q", p, got, want)
			}
		}
	}
	for _, name := range []string{"AGENTS.md", "GEMINI.md", "copilot-instructions.md"} {
		if _, err := os.Stat(filepath.Join(claude, name)); err == nil {
			t.Errorf("%s was rendered at the harness root", name)
		}
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("--check after a full sync = %d, want 0", code)
	}
}

// A hand-authored global file is kept; editing CLAUDE.md is drift --check reports in both dist and the home.
func TestInstructionRenderKeepsHandAuthoredAndGatesDrift(t *testing.T) {
	home, claude := newDocsHarness(t)
	hand := filepath.Join(home, ".codex", "AGENTS.md")
	projectWriteFile(t, hand, "# my own codex instructions\n")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if got, _ := os.ReadFile(hand); !strings.HasPrefix(string(got), "# my own") {
		t.Errorf("hand-authored ~/.codex/AGENTS.md was overwritten: %q", got)
	}
	projectWriteFile(t, filepath.Join(claude, "CLAUDE.md"), "# CLAUDE.md\n\nChanged.\n")
	if code := Sync(Options{Check: true, Out: io.Discard}); code == 0 {
		t.Error("--check after a CLAUDE.md edit = 0, want drift")
	}
}

// A bare box (no CLAUDE.md) still syncs agents and skills, just without instruction renders.
func TestSyncWithoutClaudeMDSkipsInstructionRender(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo", "demo")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "GEMINI.md")); err == nil {
		t.Error("GEMINI.md rendered without a CLAUDE.md source")
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "agents", "demo.md")); err != nil {
		t.Errorf("agent projection missing: %v", err)
	}
}

// The render must never target CLAUDE.md itself — that would overwrite the source with its own derivative.
func TestInstructionFilesNeverTargetConventionsSource(t *testing.T) {
	for vendor, rel := range instructionFiles {
		if strings.EqualFold(filepath.Base(rel), "CLAUDE.md") {
			t.Fatalf("%s instruction file is %s — the render would clobber its own source", vendor, rel)
		}
	}
}

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
	assertOrphanPrunedKeeperKept(t, home, claude, "doomed", "keeper")
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

// assertOrphanPrunedKeeperKept requires orphan gone from every vendor dist and home, and keeper still in each home.
func assertOrphanPrunedKeeperKept(t *testing.T, home, claude, orphan, keeper string) {
	t.Helper()
	for _, vendor := range Names() {
		tg := targets[vendor]
		for _, p := range []string{filepath.Join(Dir(claude, vendor), orphan+tg.ext), filepath.Join(tg.home(home), orphan+tg.ext)} {
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("orphan survived prune: %s", p)
			}
		}
		if _, err := os.Stat(filepath.Join(tg.home(home), keeper+tg.ext)); err != nil {
			t.Errorf("%s: keeper pruned: %v", vendor, err)
		}
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

var doneLine = regexp.MustCompile(`Done: (\d+) to sync, (\d+) up to date, (\d+) skipped(.*)`)

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		m[p] = fi.ModTime().String() + "|" + fi.Mode().String()
		if b, e := os.ReadFile(p); e == nil {
			m[p] += "|" + string(b)
		}
		return nil
	})
	return m
}

func TestCheckSummaryCleanFixtureReportsZeroToSync(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	writeSource(t, claude, "skills", "demo-skill", "demo-skill")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	var out bytes.Buffer
	if code := Sync(Options{Check: true, Out: &out}); code != 0 {
		t.Fatalf("check = %d\n%s", code, out.String())
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || m[1] != "0" || m[2] == "0" || m[4] != "" {
		t.Errorf("clean summary = %q\n%s", m, out.String())
	}
	if strings.Contains(out.String(), "would be") {
		t.Errorf("old wording leaked:\n%s", out.String())
	}
}

func TestCheckSummaryDriftedFixtureCountsPending(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	writeSource(t, claude, "skills", "new-skill", "new-skill")
	projectWriteFile(t, sourcePath(claude, "agents", "demo-agent"), "---\nname: demo-agent\ndescription: changed.\n---\n\nnew\n")
	var out bytes.Buffer
	if code := Sync(Options{Check: true, Out: &out}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || m[1] == "0" {
		t.Errorf("drifted summary = %q\n%s", m, out.String())
	}
}

func TestCheckSummaryShowsPruneCount(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "agents", "doomed", "doomed")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	if err := os.Remove(sourcePath(claude, "agents", "doomed")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Sync(Options{Check: true, Out: &out}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || !regexp.MustCompile(`^, \d+ to prune$`).MatchString(m[4]) {
		t.Errorf("prune suffix = %q\n%s", m, out.String())
	}
}

func TestRealSyncSummaryWordingUnchanged(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	var out bytes.Buffer
	if code := Sync(Options{Out: &out}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	if !regexp.MustCompile(`Done: \d+ synced, \d+ skipped`).MatchString(out.String()) {
		t.Errorf("real-run summary:\n%s", out.String())
	}
	if strings.Contains(out.String(), "to sync") {
		t.Errorf("dry-run wording in real run:\n%s", out.String())
	}
}

func TestCheckWritesNothing(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatal(code)
	}
	writeSource(t, claude, "skills", "new-skill", "new-skill")
	before := snapshot(t, home)
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	after := snapshot(t, home)
	if len(before) != len(after) {
		t.Errorf("file count %d -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("changed: %s", p)
		}
	}
}

// Always-on standards (no `paths:`) are appended under `## Standards` with H1s demoted; scoped ones stay out.
func TestSyncRendersAlwaysOnStandardsOnly(t *testing.T) {
	home, claude := newDocsHarness(t)
	projectWriteFile(t, filepath.Join(claude, "rules", "standards", "b.md"), "# Bee\n\nbee rule\n\n```\n# keep\n```\n")
	projectWriteFile(t, filepath.Join(claude, "rules", "standards", "a.md"), "---\nname: x\n---\n\n# Ay\n\nay rule\n")
	projectWriteFile(t, filepath.Join(claude, "rules", "standards", "scoped.md"), "---\npaths:\n  - \"**/*.md\"\n---\n\n# Scoped\n\nscoped rule\n")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("Sync exit = %d, want 0", code)
	}
	raw, err := os.ReadFile(filepath.Join(home, instructionFiles["codex"]))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "\n## Standards\n\n## Ay\n\nay rule\n\n## Bee\n\nbee rule\n\n```\n# keep\n```\n\n## Agents") {
		t.Errorf("standards section wrong:\n%s", got)
	}
	if strings.Contains(got, "scoped rule") {
		t.Error("paths-scoped standard was rendered")
	}
	projectWriteFile(t, filepath.Join(claude, "rules", "standards", "a.md"), "# Ay\n\nchanged\n")
	if code := Sync(Options{Check: true, Out: io.Discard}); code == 0 {
		t.Error("--check after a standards edit = 0, want drift")
	}
}
