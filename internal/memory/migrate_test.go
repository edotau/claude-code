package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// canonTemp resolves p (a path t.TempDir() or a real dir already created) the same way register()
// does internally, so fixture-built directory names match what Inventory resolves at runtime.
func canonTemp(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// rawGit runs git directly against dir, bypassing the package's own sanitized Repo.run — used only to
// build/inspect fixture repositories (R, the decoy) that the migration engine itself must never touch.
func rawGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(gitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeClaudeJSON seeds a minimal ~/.claude.json with one projects entry, as claudeJSONProjects reads it.
func writeClaudeJSON(t *testing.T, cfg string, repoPaths ...string) {
	t.Helper()
	projects := map[string]any{}
	for _, p := range repoPaths {
		projects[p] = map[string]any{}
	}
	data, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(cfg, ".claude.json"), string(data))
}

// migrateConfig isolates CLAUDE_CONFIG_DIR and HOME (and XDG_CONFIG_HOME) under fresh temp dirs.
func migrateConfig(t *testing.T) (cfg, home string) {
	t.Helper()
	cfg = canonTemp(t, t.TempDir())
	home = canonTemp(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return cfg, home
}

// inventoryAndWrite runs Inventory and WriteManifest, failing the test on either error — the two-step
// dry run every apply test needs before it can touch Apply at all.
func inventoryAndWrite(t *testing.T, o InventoryOptions) (Manifest, string) {
	t.Helper()
	m, err := Inventory(o)
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, path
}

// minimalManifest writes n typed global bank files with distinguishable content and returns the
// resulting manifest and its written path, for scenarios that need a small, fully predictable item
// list instead of the full legacyFixture.
func minimalManifest(t *testing.T, names ...string) (Manifest, string) {
	t.Helper()
	for _, name := range names {
		write(t, filepath.Join(LegacyGlobalDir(), name), "# "+name+"\nminimal real content\n")
	}
	return inventoryAndWrite(t, InventoryOptions{})
}

// legacyFixture is the full plan §4 inventory: a real repo R, its native store, the legacy global bank
// (six typed files, archive, index, access log, lock file), a changed+resolved repo bank, a pristine
// bank, a quarantine-worthy junk dir, an unresolvable-but-pristine junk dir, an unresolved+changed
// orphan bank, docs/handoffs artefacts, the harness's and $HOME's own native stores, and an outer decoy
// git repo rooted at CLAUDE_CONFIG_DIR itself with one staged, uncommitted file.
type legacyFixture struct {
	cfg, home  string
	repoR      string
	rSlug      string
	orphanDir  string
	impeccable string
	decoy      string // CLAUDE_CONFIG_DIR, also a git repo
}

func newLegacyFixture(t *testing.T, withOrcaAlias bool) legacyFixture {
	t.Helper()
	cfg, home := migrateConfig(t)
	buildDecoyRepo(t, cfg)

	repoR := canonTemp(t, gitRepo(t, t.TempDir(), "repo-r"))
	writeClaudeJSON(t, cfg, repoR)
	if withOrcaAlias {
		if err := os.MkdirAll(filepath.Join(home, "orca", "claude-code"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	rSlug := paths.RepoSlug(repoR)
	buildNativeStore(t, filepath.Join(cfg, "projects", projectSlug(repoR), "memory"))
	buildLegacyGlobalBank(t)
	buildChangedRepoBank(t, rSlug)
	buildPristineBank(t, filepath.Join(LegacyGlobalDir(), "pristine1"))
	impeccable := filepath.Join(LegacyGlobalDir(), ".impeccable", "hook.cache.json")
	write(t, impeccable, strings.Repeat("x", 27)) // unknown junk: quarantined, not deleted

	// `ling/`: pristine templates, but no origin anywhere — still deleted, no finding needed.
	buildPristineBank(t, filepath.Join(LegacyGlobalDir(), "ling"))

	// `orphan-repo/`: changed (non-pristine) content with NO resolvable origin -> unresolved finding + skip.
	orphanDir := filepath.Join(LegacyGlobalDir(), "orphan-repo")
	for name := range templates() {
		write(t, filepath.Join(orphanDir, name), "# orphan "+name+"\nchanged, unresolved\n")
	}

	buildHandoffs(t, cfg)
	write(t, filepath.Join(cfg, "projects", projectSlug(cfg), "memory", "cfg-topic.md"),
		"---\nname: cfg-topic\n---\n\nharness fact\n")
	write(t, filepath.Join(cfg, "projects", projectSlug(home), "memory", "home-topic.md"),
		"---\nname: home-topic\n---\n\nhome fact\n")

	return legacyFixture{cfg: cfg, home: home, repoR: repoR, rSlug: rSlug, orphanDir: orphanDir, impeccable: impeccable, decoy: cfg}
}

// buildDecoyRepo makes dir itself a git repo with one staged, uncommitted file — the outer tree the
// migration engine's own nested memory repo must never touch (it always passes explicit --git-dir).
func buildDecoyRepo(t *testing.T, dir string) {
	t.Helper()
	rawGit(t, dir, "init", "-q", "-b", "main")
	rawGit(t, dir, "config", "user.email", "decoy@localhost")
	rawGit(t, dir, "config", "user.name", "decoy")
	write(t, filepath.Join(dir, "staged.md"), "staged outer content\n")
	rawGit(t, dir, "add", "staged.md")
}

// buildNativeStore seeds a real (non-symlinked) native auto-memory dir: a topic, MEMORY.md,
// SEARCHABLE.md and one archived topic.
func buildNativeStore(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, "a-topic.md"), "---\nname: a-topic\n---\n\nbody\n")
	write(t, filepath.Join(dir, NativeIndexFile), "# Memory Index\n\n- [A topic](a-topic.md) — note\n")
	write(t, filepath.Join(dir, NativeSearchableFile), "- [Old](old.md) — pointer\n")
	write(t, filepath.Join(dir, "archive", "old.md"), "archived\n")
}

// buildLegacyGlobalBank seeds LegacyGlobalDir() with the six typed files, an archived sessionHistory, the
// stale index, an access log and a stale lock file.
func buildLegacyGlobalBank(t *testing.T) {
	t.Helper()
	g := LegacyGlobalDir()
	for name := range templates() {
		write(t, filepath.Join(g, name), "# global "+name+"\nreal content\n")
	}
	write(t, filepath.Join(g, "archive", "sessionHistory-2026-01.md"), "old history\n")
	write(t, filepath.Join(g, CatalogFile), "stale catalog\n")
	write(t, filepath.Join(g, "access.log"), "2026-01-01T00:00:00Z -- read -- test\n")
	write(t, filepath.Join(g, ".postharvest.lock"), "")
}

// buildChangedRepoBank seeds LegacyGlobalDir()/slug/ with changed (non-template) typed files plus an archive.
func buildChangedRepoBank(t *testing.T, slug string) {
	t.Helper()
	dir := filepath.Join(LegacyGlobalDir(), slug)
	for name := range templates() {
		write(t, filepath.Join(dir, name), "# "+name+" for "+slug+"\nchanged\n")
	}
	write(t, filepath.Join(dir, "archive", "old-decision.md"), "archived decision\n")
}

// buildPristineBank seeds dir with the six typed files byte-equal to their templates.
func buildPristineBank(t *testing.T, dir string) {
	t.Helper()
	for name, body := range templates() {
		write(t, filepath.Join(dir, name), body)
	}
}

// buildHandoffs seeds cfg/docs/handoffs with the plan §4-named entries: the preserved handoff, the stale
// beacon, and both run artefacts authorized for deletion.
func buildHandoffs(t *testing.T, cfg string) {
	t.Helper()
	dir := filepath.Join(cfg, "docs", "handoffs")
	write(t, filepath.Join(dir, "2026-10-17-loop-workflows.md"), "# handoff\nbody\n")
	write(t, filepath.Join(dir, "HANDOFF.md"), "stale beacon\n")
	write(t, filepath.Join(dir, "2026-10-17-loop-workflows.plan.json"), "{}")
	write(t, filepath.Join(dir, "2026-10-17-loop-workflows.critic.json"), "{}")
}

func findItem(items []MigrateItem, srcSuffix string) (MigrateItem, bool) {
	for _, it := range items {
		if it.Source != "" && strings.HasSuffix(it.Source, srcSuffix) {
			return it, true
		}
	}
	return MigrateItem{}, false
}

// wantItem asserts an item for srcSuffix exists with the given Kind and Action, returning it for any
// further field-specific checks the caller needs.
func wantItem(t *testing.T, items []MigrateItem, srcSuffix, kind, action string) MigrateItem {
	t.Helper()
	it, ok := findItem(items, srcSuffix)
	if !ok || it.Kind != kind || it.Action != action {
		t.Errorf("item for %q = %+v (found=%v), want kind=%s action=%s", srcSuffix, it, ok, kind, action)
	}
	return it
}

func containsFinding(findings []string, sub string) bool {
	for _, f := range findings {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}

// Case 1: Inventory is a pure read: no store, no state, sources untouched; the manifest classifies by
// rule, Findings name the unresolved slug and a quarantine, and an --origin pin resolves it on a rerun.
func TestInventoryReadOnlyAndFindings(t *testing.T) {
	fx := newLegacyFixture(t, false)

	m, err := Inventory(InventoryOptions{Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	assertPathsGone(t, StoreDir(), MigrateStateDir()) // read-only: no store, no state
	if _, err := os.Stat(fx.impeccable); err != nil {
		t.Error("Inventory must leave sources untouched")
	}

	global := wantItem(t, m.Items, filepath.Join("docs", "memory", "bank", "activeContext.md"), KindGlobalBank, ActionCopy)
	if global.Dest != filepath.Join(StoreDir(), "home", "activeContext.md") {
		t.Errorf("global activeContext.md dest = %s", global.Dest)
	}
	wantItem(t, m.Items, "a-topic.md", KindNativeTopic, ActionCopy)
	wantItem(t, m.Items, "hook.cache.json", KindUnknownItem, ActionQuarantine)
	wantItem(t, m.Items, filepath.Join("orphan-repo", "activeContext.md"), KindUnknownItem, ActionSkip)
	if !containsFinding(m.Findings, "no origin for legacy bank orphan-repo") {
		t.Errorf("findings missing unresolved slug notice: %v", m.Findings)
	}
	if !containsFinding(m.Findings, "quarantine") {
		t.Errorf("findings missing a quarantine notice: %v", m.Findings)
	}

	// A second run with an --origin pin resolves the orphan.
	origin := canonTemp(t, gitRepo(t, t.TempDir(), "orphan-origin"))
	m2, err := Inventory(InventoryOptions{Origins: map[string]string{"orphan-repo": origin}})
	if err != nil {
		t.Fatalf("Inventory with --origin: %v", err)
	}
	if containsFinding(m2.Findings, "no origin for legacy bank orphan-repo") {
		t.Errorf("--origin pin did not resolve orphan-repo: %v", m2.Findings)
	}
	wantItem(t, m2.Items, filepath.Join("orphan-repo", "activeContext.md"), KindRepoBank, ActionCopy)
	if m2.Origins["orphan-repo"] != origin {
		t.Errorf("Origins[orphan-repo] = %s want %s", m2.Origins["orphan-repo"], origin)
	}
}

// Case 2: WriteManifest/LoadManifest round-trip, and a manifest written for a foreign ConfigDir is stale.
func TestManifestWriteLoadRoundTrip(t *testing.T) {
	newLegacyFixture(t, false)
	m, err := Inventory(InventoryOptions{Now: time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != m.Schema || got.ConfigDir != m.ConfigDir || len(got.Items) != len(m.Items) {
		t.Errorf("round-trip mismatch: %+v vs original len %d", got, len(m.Items))
	}

	foreign := m
	foreign.ConfigDir = t.TempDir()
	data, err := json.MarshalIndent(foreign, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(t.TempDir(), "foreign.json")
	if err := os.WriteFile(foreignPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(foreignPath); !isManifestStale(err) {
		t.Errorf("LoadManifest on a foreign ConfigDir err = %v, want ErrManifestStale", err)
	}
}

func isManifestStale(err error) bool {
	for err != nil {
		if err == ErrManifestStale {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// assertHashMatchesItem re-hashes dest and compares it against the manifest item for srcSuffix. A typed
// bank file's dest carries the settle phase's prepended frontmatter block, so it hashes the remainder.
func assertHashMatchesItem(t *testing.T, items []MigrateItem, srcSuffix, dest string) {
	t.Helper()
	it, ok := findItem(items, srcSuffix)
	if !ok {
		t.Fatalf("no item for %s", srcSuffix)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Errorf("%s: %v", dest, err)
		return
	}
	if it.Kind == KindGlobalBank || it.Kind == KindRepoBank {
		data = data[len(bankFrontmatter(filepath.Base(dest))):]
	}
	if sum := sha256Hex(data); sum != it.SHA256 {
		t.Errorf("%s hash = %s want %s", dest, sum, it.SHA256)
	}
}

// assertOriginAliases checks dir's .origin carries exactly want aliases.
func assertOriginAliases(t *testing.T, dir string, want int) {
	t.Helper()
	origin, err := ReadOrigin(dir)
	if err != nil {
		t.Fatalf("ReadOrigin(%s): %v", dir, err)
	}
	if len(origin.Aliases) != want {
		t.Errorf("%s .origin aliases = %v, want %d", dir, origin.Aliases, want)
	}
}

// assertStoreCommitCoversImports checks the store's committed tree includes every copied item's rel
// path plus at least one migrate receipt.
func assertStoreCommitCoversImports(t *testing.T, items []MigrateItem) {
	t.Helper()
	lsOut, err := StoreRepo().run("ls-files")
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	for l := range strings.SplitSeq(lsOut, "\n") {
		tracked[l] = true
	}
	for _, it := range items {
		if it.Action != ActionCopy || it.Dest == "" || it.Kind == KindOriginItem || it.Inline != nil {
			continue // an origin item's Dest is the store dir itself (a vault row, not a tracked file)
		}
		if rel, ok := InsideStore(it.Dest); ok && !tracked[rel] {
			t.Errorf("ls-files missing imported rel %s", rel)
		}
	}
	for rel := range tracked {
		if strings.HasPrefix(rel, "receipts/migrate-") {
			return
		}
	}
	t.Error("ls-files missing a receipts/migrate-*.json entry")
}

// assertPathsGone checks every path in paths no longer exists.
func assertPathsGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected gone, still present: %s", p)
		}
	}
}

// assertQuarantinedByteEqual checks exactly one quarantined copy of name exists under state, with
// bytes equal to want.
func assertQuarantinedByteEqual(t *testing.T, name, want string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(MigrateStateDir(), "quarantine", "*", "*", name))
	if len(matches) != 1 {
		t.Fatalf("quarantined %s matches = %v", name, matches)
	}
	if data, err := os.ReadFile(matches[0]); err != nil || string(data) != want {
		t.Errorf("quarantined %s bytes = %q,%v", name, data, err)
	}
}

// assertDecoyIndexUnchanged re-reads decoy's index and HEAD ref and compares them against the before snapshot.
func assertDecoyIndexUnchanged(t *testing.T, decoy, statusBefore, refBefore string) {
	t.Helper()
	if got := rawGit(t, decoy, "status", "--porcelain", "--untracked-files=no"); got != statusBefore {
		t.Errorf("decoy repo index changed:\nbefore %q\nafter  %q", statusBefore, got)
	}
	if got := rawGit(t, decoy, "symbolic-ref", "HEAD"); got != refBefore {
		t.Errorf("decoy repo HEAD moved: %q -> %q", refBefore, got)
	}
}

// Case 3: a full apply copies every item, writes .origin (with the orca alias only when present),
// commits a baseline whose ls-files covers every imported rel plus the receipt, retires sources,
// deletes pristine banks, quarantines junk byte-equal, drops handoff artefacts, moves access logs,
// never touches the decoy repo, clears the maintenance marker, and reports Complete.
func TestApplyFullImportCommitsAndRetires(t *testing.T) {
	for _, withAlias := range []bool{false, true} {
		t.Run(map[bool]string{false: "noAlias", true: "withAlias"}[withAlias], func(t *testing.T) {
			fx := newLegacyFixture(t, withAlias)
			m, manifestPath := inventoryAndWrite(t, InventoryOptions{})
			statusBefore := rawGit(t, fx.decoy, "status", "--porcelain", "--untracked-files=no")
			refBefore := rawGit(t, fx.decoy, "symbolic-ref", "HEAD")

			var out bytes.Buffer
			res, err := Apply(manifestPath, &out)
			if err != nil {
				t.Fatalf("Apply: %v\n%s", err, out.String())
			}
			if !res.Complete {
				t.Errorf("Complete = false, want true (skips must not block it): %+v", res)
			}

			assertHashMatchesItem(t, m.Items, filepath.Join("docs", "memory", "bank", "activeContext.md"),
				filepath.Join(StoreDir(), "home", "activeContext.md"))

			wantAliases := 0
			if withAlias {
				wantAliases = 1
			}
			assertOriginAliases(t, HarnessStoreDir(), wantAliases)
			assertStoreCommitCoversImports(t, m.Items)

			assertPathsGone(t,
				filepath.Join(LegacyGlobalDir(), "activeContext.md"),
				filepath.Join(LegacyGlobalDir(), fx.rSlug, "activeContext.md"),
				filepath.Join(fx.cfg, "projects", projectSlug(fx.repoR), "memory", "a-topic.md"),
				filepath.Join(LegacyGlobalDir(), "pristine1"),
				filepath.Join(LegacyGlobalDir(), "ling"),
				filepath.Join(fx.cfg, "docs", "handoffs", "HANDOFF.md"),
				filepath.Join(fx.cfg, "docs", "handoffs", "2026-10-17-loop-workflows.plan.json"),
				filepath.Join(fx.cfg, "docs", "handoffs", "2026-10-17-loop-workflows.critic.json"),
			)
			assertQuarantinedByteEqual(t, "hook.cache.json", strings.Repeat("x", 27))

			handoff := filepath.Join(HandoffsDir(HarnessStoreDir()), "2026-10-17-loop-workflows.md")
			if data, err := os.ReadFile(handoff); err != nil || string(data) != "# handoff\nbody\n" {
				t.Errorf("handoff not landed at %s: %v %q", handoff, err, data)
			}
			accessLog := filepath.Join(StateDir(), "access", "home.log")
			if _, err := os.Stat(accessLog); err != nil {
				t.Errorf("access log not moved to %s: %v", accessLog, err)
			}

			assertDecoyIndexUnchanged(t, fx.decoy, statusBefore, refBefore)

			if _, err := os.Stat(MaintenancePath()); !os.IsNotExist(err) {
				t.Error("maintenance marker left up after a successful apply")
			}
		})
	}
}

// readJournal loads and parses the migration journal at path, failing the test on any error.
func readJournal(t *testing.T, path string) migrateJournal {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	var j migrateJournal
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// assertAllHashesMatch re-hashes every ActionCopy item's Dest (excluding inline .origin records, which
// WriteOrigin regenerates rather than byte-copies) and compares it to the manifest's recorded SHA256.
func assertAllHashesMatch(t *testing.T, items []MigrateItem) {
	t.Helper()
	for _, it := range items {
		if it.Action != ActionCopy || it.Inline != nil {
			continue
		}
		data, err := os.ReadFile(it.Dest)
		if err != nil {
			t.Errorf("%s: %v", it.Dest, err)
			continue
		}
		if it.Kind == KindGlobalBank || it.Kind == KindRepoBank {
			data = data[len(bankFrontmatter(filepath.Base(it.Dest))):]
		}
		if sum := sha256Hex(data); sum != it.SHA256 {
			t.Errorf("%s hash = %s want %s", it.Dest, sum, it.SHA256)
		}
	}
}

// injectAfterTwoCopies writes a 3-item manifest, runs Apply with applyFailAfter=2 (restored on
// return), and asserts the common shape of the resulting failure: the injected error, a copy-phase
// journal with exactly 2 steps, and no leftover maintenance marker. Shared by every resume test below.
func injectAfterTwoCopies(t *testing.T) (m Manifest, manifestPath string, journal migrateJournal) {
	t.Helper()
	migrateConfig(t)
	m, manifestPath = minimalManifest(t, "activeContext.md", "decisionLog.md", "progress.md")

	applyFailAfter = 2
	t.Cleanup(func() { applyFailAfter = 0 })

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("first Apply err = %v, want injected failure", err)
	}
	journal = readJournal(t, res.Journal)
	if journal.Phase != phaseCopy || len(journal.Steps) != 2 {
		t.Fatalf("journal after failure: phase=%s steps=%d, want copy/2", journal.Phase, len(journal.Steps))
	}
	if _, err := os.Stat(MaintenancePath()); !os.IsNotExist(err) {
		t.Error("maintenance marker left up after a failed apply")
	}
	applyFailAfter = 0 // the caller's resume must not re-trigger the injected failure
	return m, manifestPath, journal
}

// Case 4: an injected failure mid-copy leaves a resumable journal; re-running finishes it, resumes the
// already-copied items (never re-copying or re-committing them), and commits exactly once.
func TestApplyResumesAfterInjectedFailure(t *testing.T) {
	m, manifestPath, _ := injectAfterTwoCopies(t)
	var out2 bytes.Buffer
	res2, err := Apply(manifestPath, &out2)
	if err != nil {
		t.Fatalf("resumed Apply: %v\n%s", err, out2.String())
	}
	if res2.Resumed != 2 {
		t.Errorf("Resumed = %d, want 2", res2.Resumed)
	}
	if !res2.Complete {
		t.Error("resumed apply must complete")
	}
	// Import and settle (the frontmatter stamp) commit separately by design: two commits, not one.
	if commits, err := StoreRepo().Log(10); err != nil || len(commits) != 2 {
		t.Errorf("commits = %v, %v, want exactly two", commits, err)
	}
	if receipts, _ := filepath.Glob(filepath.Join(ReceiptsDir(), "migrate-*.json")); len(receipts) != 1 {
		t.Errorf("receipts = %v, want exactly one", receipts)
	}
	assertAllHashesMatch(t, m.Items)
}

// Regression: resumed() must rehash the dest, not just trust the journal's recorded state — a dest
// tampered during the crash gap is a conflict on resume, never silently re-trusted or overwritten.
func TestApplyResumeRevalidatesTamperedDest(t *testing.T) {
	m, manifestPath, j := injectAfterTwoCopies(t)

	var tampered MigrateItem
	for _, it := range m.Items {
		if it.Kind == KindOriginItem || it.Inline != nil {
			continue // its Dest is the store dir itself, not a file to tamper with
		}
		if st, ok := j.Steps[it.ID]; ok && st.State == stepCopied {
			tampered = it
			break
		}
	}
	if tampered.Dest == "" {
		t.Fatalf("no copied item found in journal: %+v", j.Steps)
	}
	write(t, tampered.Dest, "corrupted during the crash gap\n")

	var out2 bytes.Buffer
	res2, err := Apply(manifestPath, &out2)
	if err != nil {
		t.Fatalf("resumed Apply: %v\n%s", err, out2.String())
	}
	if res2.Complete {
		t.Error("Complete must be false when a resumed dest was tampered")
	}
	if len(res2.Conflicts) != 1 || !strings.Contains(res2.Conflicts[0], tampered.ID) {
		t.Fatalf("Conflicts = %v, want exactly the tampered item %s", res2.Conflicts, tampered.ID)
	}
	if res2.Resumed != 1 {
		t.Errorf("Resumed = %d, want exactly 1 (the untouched copied item)", res2.Resumed)
	}
	if data, err := os.ReadFile(tampered.Dest); err != nil || string(data) != "corrupted during the crash gap\n" {
		t.Error("a tampered dest must be left untouched, never silently overwritten or re-trusted")
	}
}

// Case 5: a source edited after the dry run is reported Changed, left in place, and blocks Complete.
func TestApplyDetectsChangedSourceSinceDryRun(t *testing.T) {
	migrateConfig(t)
	_, manifestPath := minimalManifest(t, "activeContext.md")

	src := filepath.Join(LegacyGlobalDir(), "activeContext.md")
	write(t, src, "# activeContext.md\nEDITED after the dry run\n")

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if len(res.Changed) != 1 {
		t.Fatalf("Changed = %v, want exactly one entry", res.Changed)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("changed source must be left in place")
	}
	if res.Complete {
		t.Error("Complete must be false when a source changed")
	}
}

// Case 6: a dest that already holds the same bytes stays a copy (noted at dry-run time) so apply still
// verifies it; a dest that diverges only between dry-run and apply is reported as an apply-time Conflict
// and both copies are kept untouched.
func TestApplyHandlesPreexistingDestConflictAndEqualBytes(t *testing.T) {
	migrateConfig(t)
	write(t, filepath.Join(LegacyGlobalDir(), "activeContext.md"), "# activeContext.md\nminimal real content\n")
	write(t, filepath.Join(LegacyGlobalDir(), "progress.md"), "# progress.md\nminimal real content\n")

	// activeContext.md's dest already holds the exact same bytes as its (about to be hashed) source.
	srcABytes, err := os.ReadFile(filepath.Join(LegacyGlobalDir(), "activeContext.md"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(homeStoreDir(), "activeContext.md"), string(srcABytes))

	m, manifestPath := inventoryAndWrite(t, InventoryOptions{})
	already := wantItem(t, m.Items, "activeContext.md", KindGlobalBank, ActionCopy)
	if !strings.Contains(already.Reason, "equal bytes") || already.Dest == "" {
		t.Errorf("activeContext.md = %+v, want a copy whose reason notes the equal dest", already)
	}

	// progress.md's dest appears with DIFFERENT bytes only after the manifest was written (a race).
	write(t, filepath.Join(homeStoreDir(), "progress.md"), "a completely different file\n")
	srcB := filepath.Join(LegacyGlobalDir(), "progress.md")

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v, want exactly one", res.Conflicts)
	}
	if _, err := os.Stat(srcB); err != nil {
		t.Error("conflicting source must be kept")
	}
	if data, err := os.ReadFile(filepath.Join(homeStoreDir(), "progress.md")); err != nil || string(data) != "a completely different file\n" {
		t.Error("conflicting dest must be kept untouched")
	}
}

// Case 6b: an equal-bytes dest is no shortcut: apply re-hashes it, commits it into the store and retires
// the source, so a dest edited after the dry run is caught as a conflict instead of accepted as done.
func TestApplyEqualDestVerifiesCommitsAndRetires(t *testing.T) {
	// seed isolates a fresh box whose dest already holds the source's bytes, returning the dry-run manifest.
	seed := func() (src, dest string, m Manifest, manifestPath string) {
		migrateConfig(t)
		src, dest = filepath.Join(LegacyGlobalDir(), "activeContext.md"), filepath.Join(homeStoreDir(), "activeContext.md")
		write(t, src, "# activeContext.md\nminimal real content\n")
		write(t, dest, "# activeContext.md\nminimal real content\n")
		m, manifestPath = inventoryAndWrite(t, InventoryOptions{})
		return src, dest, m, manifestPath
	}
	src, _, m, manifestPath := seed()
	wantItem(t, m.Items, "activeContext.md", KindGlobalBank, ActionCopy)

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if !res.Complete || res.Commit == "" {
		t.Fatalf("Complete = %v, Commit = %q, want a complete, committed apply:\n%s", res.Complete, res.Commit, out.String())
	}
	assertStoreCommitCoversImports(t, m.Items)
	assertPathsGone(t, src)

	// Same setup on a fresh box, but the dest is edited after the dry run: a conflict, never a silent skip.
	src, dest, _, manifestPath := seed()
	write(t, dest, "edited after the dry run\n")
	out.Reset()
	res, err = Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if res.Complete || len(res.Conflicts) != 1 {
		t.Errorf("Complete = %v, Conflicts = %v, want blocked by exactly one conflict", res.Complete, res.Conflicts)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("conflicting source must be kept")
	}
}

func isNotQuiesced(err error) bool {
	for err != nil {
		if err == ErrNotQuiesced {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Case 7: live harness claims (a watcher claim, or any pending harvest) refuse the apply outright, writing
// nothing; a pending harvest past the grace window is an unknown claim to diagnose (plan §4), not a dead one.
func TestApplyRefusesWhileNotQuiesced(t *testing.T) {
	t.Run("watchClaim", func(t *testing.T) {
		migrateConfig(t)
		_, manifestPath := minimalManifest(t, "activeContext.md")
		write(t, filepath.Join(StateDir(), "watch-s1"), "")

		var out bytes.Buffer
		res, err := Apply(manifestPath, &out)
		if !isNotQuiesced(err) {
			t.Fatalf("Apply err = %v, want ErrNotQuiesced", err)
		}
		// A refused apply must raise no marker, create no store, write no journal.
		assertPathsGone(t, MaintenancePath(), StoreDir(), res.Journal)
	})

	for _, tc := range []struct {
		name, status string
		age          time.Duration
		blocks       bool
	}{
		{"freshPendingHarvestBlocks", HarvestPending, 0, true},
		{"oldPendingHarvestBlocks", HarvestPending, 2 * harvestGrace, true},
		{"resolvedHarvestDoesNotBlock", HarvestFailed, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			migrateConfig(t)
			_, manifestPath := minimalManifest(t, "activeContext.md")
			if err := WriteHarvestResult("s2", HarvestResult{Status: tc.status, Time: time.Now().Add(-tc.age)}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			res, err := Apply(manifestPath, &out)
			if !tc.blocks {
				if err != nil {
					t.Fatalf("a resolved harvest must not block apply: %v\n%s", err, out.String())
				}
				return
			}
			if !isNotQuiesced(err) || !strings.Contains(err.Error(), "harvest-result-s2") {
				t.Fatalf("Apply err = %v, want ErrNotQuiesced naming harvest-result-s2", err)
			}
			assertPathsGone(t, MaintenancePath(), StoreDir(), res.Journal)
		})
	}
}

// Case 8: the bank lock held by another worker refuses the apply with ErrBankBusy, and the maintenance
// marker it briefly raised is cleared again.
func TestApplyRefusesWhenBankLockBusy(t *testing.T) {
	shortBankWait(t)
	migrateConfig(t)
	_, manifestPath := minimalManifest(t, "activeContext.md")

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(BankLockPath(), 3*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	var out bytes.Buffer
	if _, err := Apply(manifestPath, &out); err != ErrBankBusy {
		t.Fatalf("Apply err = %v, want ErrBankBusy", err)
	}
	if _, err := os.Stat(MaintenancePath()); !os.IsNotExist(err) {
		t.Error("maintenance marker left up after ErrBankBusy")
	}
}

// Regression: a typed-name collision that only appears AFTER the dry run (a typed file created between
// manifest write and apply) must still block — copyExclusive's O_EXCL never overwrites it, even though
// the manifest recorded ActionCopy when no typed file existed yet.
func TestApplyBlocksTypedNameCollisionAppearingAfterDryRun(t *testing.T) {
	cfg, _ := migrateConfig(t)
	repoR := canonTemp(t, gitRepo(t, t.TempDir(), "repo-r"))
	writeClaudeJSON(t, cfg, repoR)
	dest, err := EnsureRepoDir(repoR)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(cfg, "projects", projectSlug(repoR), "memory", "progress.md"),
		"a native file that happens to share the typed name\n")

	m, manifestPath := inventoryAndWrite(t, InventoryOptions{})
	it := wantItem(t, m.Items, filepath.Join(projectSlug(repoR), "memory", "progress.md"), KindNativeTopic, ActionCopy)
	if !strings.Contains(it.Reason, "no typed file exists") {
		t.Fatalf("reason = %q, want it to note no typed file existed yet", it.Reason)
	}

	// The race: a real typed progress.md is created only now, after the manifest was already written.
	write(t, filepath.Join(dest, "progress.md"), "# Progress\nwritten after the dry run\n")

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("Conflicts = %v, want exactly one", res.Conflicts)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "progress.md")); err != nil || string(data) != "# Progress\nwritten after the dry run\n" {
		t.Fatalf("typed file was overwritten: %q, %v", data, err)
	}
	if _, err := os.Stat(it.Source); err != nil {
		t.Error("blocked native source must be left in place")
	}
}

// Case 9: a native store that is already a symlink is reported as already-linked and generates no items.
func TestInventorySkipsAlreadyLinkedNativeStore(t *testing.T) {
	cfg, _ := migrateConfig(t)
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, "topic.md"), "---\nname: topic\n---\n\nbody\n")
	link := filepath.Join(cfg, "projects", "some-slug", "memory")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}

	m, err := Inventory(InventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsFinding(m.Findings, "already linked: "+link) {
		t.Errorf("findings missing already-linked notice: %v", m.Findings)
	}
	if _, ok := findItem(m.Items, "topic.md"); ok {
		t.Error("a symlinked native store must not generate any items")
	}
}

// Case 10: a native topic named like a typed file (e.g. progress.md) whose typed destination already
// exists is a destless conflict: never overwritten, and apply stays blocked until a new manifest resolves it.
func TestInventoryTypedNameCollisionBlocksApply(t *testing.T) {
	cfg, _ := migrateConfig(t)
	repoR := canonTemp(t, gitRepo(t, t.TempDir(), "repo-r"))
	writeClaudeJSON(t, cfg, repoR)

	dest, err := EnsureRepoDir(repoR) // RepoDir alone would hash past a dir lacking .origin (identity.go collision guard)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dest, "progress.md"), "# Progress\nalready present typed file\n")
	write(t, filepath.Join(cfg, "projects", projectSlug(repoR), "memory", "progress.md"),
		"a native file that happens to share the typed name\n")

	m, manifestPath := inventoryAndWrite(t, InventoryOptions{})
	it := wantItem(t, m.Items, filepath.Join(projectSlug(repoR), "memory", "progress.md"), KindNativeTopic, ActionSkip)
	if !strings.HasPrefix(it.Reason, "conflict:") || !strings.Contains(it.Reason, "typed-name collision") || it.Dest != "" {
		t.Errorf("colliding native progress.md = %+v, want a destless conflict", it)
	}
	if !containsFinding(m.Findings, "typed-name collision") {
		t.Errorf("findings missing typed-name-collision notice: %v", m.Findings)
	}

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("Apply: %v\n%s", err, out.String())
	}
	if res.Complete || len(res.Conflicts) != 1 || !strings.Contains(res.Conflicts[0], it.ID) {
		t.Errorf("Complete = %v, Conflicts = %v, want blocked by the colliding item %s", res.Complete, res.Conflicts, it.ID)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "progress.md")); err != nil || string(data) != "# Progress\nalready present typed file\n" {
		t.Errorf("typed file was overwritten: %q, %v", data, err)
	}
	if _, err := os.Stat(it.Source); err != nil {
		t.Error("colliding native source must be left in place")
	}
}

// Finding 1 regression: a dest whose ancestor is a symlink (store archive/ or state access/) is rejected before any
// byte is copied — the item is a conflict, the source stays, and nothing lands behind the link.
func TestApplyRejectsDestUnderSymlinkedAncestor(t *testing.T) {
	cases := []struct {
		name, src, body string
		link            func() string
	}{
		{"globalArchive", filepath.Join("archive", "note.md"), "archived note\n", func() string { return ArchiveDir(filepath.Join(StoreDir(), "home")) }},
		{"stateAccessLog", "access.log", "2026-01-01T00:00:00Z -- read -- x\n", func() string { return filepath.Join(StateDir(), "access") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			migrateConfig(t)
			src := filepath.Join(LegacyGlobalDir(), tc.src)
			write(t, src, tc.body)
			elsewhere := t.TempDir()
			link := tc.link()
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
			m, manifestPath := inventoryAndWrite(t, InventoryOptions{})
			it, ok := findItem(m.Items, tc.src)
			if !ok || it.Action != ActionCopy {
				t.Fatalf("item = %+v found=%v, want a copy", it, ok)
			}

			var out bytes.Buffer
			res, err := Apply(manifestPath, &out)
			if err != nil {
				t.Fatalf("Apply: %v\n%s", err, out.String())
			}
			if res.Complete || len(res.Conflicts) != 1 || !strings.Contains(res.Conflicts[0], it.ID) {
				t.Fatalf("Complete=%v Conflicts=%v, want the escaping dest as the one conflict", res.Complete, res.Conflicts)
			}
			if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
				t.Errorf("bytes landed behind the symlink: %v", ents)
			}
			if _, err := os.Stat(src); err != nil {
				t.Error("rejected import's source must be left in place")
			}
		})
	}
}

// Finding 2 regression: a crash between the settle transaction's write and its commit leaves a pending txn journal
// and an uncommitted frontmatter block; the next apply must finish that journal, never fail HEAD verification forever.
func TestApplySettleResumesUncommittedTransaction(t *testing.T) {
	migrateConfig(t)
	m, manifestPath := minimalManifest(t, "activeContext.md")
	msum, _, err := sha256File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	jpath := filepath.Join(MigrateStateDir(), filepath.Base(manifestPath)+".journal.json")
	j, err := loadOrStartJournal(jpath, manifestPath, msum)
	if err != nil {
		t.Fatal(err)
	}
	a := &applier{m: m, j: j, jpath: jpath, base: filepath.Base(manifestPath), out: &bytes.Buffer{}, res: &ApplyResult{}}
	repo := StoreRepo()
	err = WithBankLock(0, func() error {
		for _, step := range []func() error{repo.Ensure, a.copyPhase, func() error { return a.advance(phaseCommit) },
			func() error { return a.commitPhase(repo) }, func() error { return a.advance(phaseSettle) }} {
			if err := step(); err != nil {
				return err
			}
		}
		changes, _, err := a.settleChanges()
		if err != nil || len(changes) != 1 {
			t.Fatalf("settleChanges = %d, %v", len(changes), err)
		}
		tj, err := newJournal(repo, "migrate: settle "+a.base, changes)
		if err != nil {
			return err
		}
		if err := tj.save(); err != nil {
			return err
		}
		if err := paths.AtomicWrite(filepath.Join(repo.Dir, changes[0].Rel), changes[0].Data, 0o600); err != nil {
			return err
		}
		tj.State = "applied" // the crash: written, journaled as applied, never committed
		return tj.save()
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Apply(manifestPath, &out)
	if err != nil {
		t.Fatalf("resumed Apply: %v\n%s", err, out.String())
	}
	if !res.Complete {
		t.Fatalf("resumed apply must complete: %+v\n%s", res, out.String())
	}
	if commits, err := StoreRepo().Log(10); err != nil || len(commits) != 2 {
		t.Errorf("commits = %v, %v, want baseline + settle", commits, err)
	}
	if left, _ := filepath.Glob(filepath.Join(JournalDir(), "*.json")); len(left) != 0 {
		t.Errorf("transaction journal left pending: %v", left)
	}
	assertAllHashesMatch(t, m.Items)
	assertPathsGone(t, filepath.Join(LegacyGlobalDir(), "activeContext.md"))
}

// Finding 3 regression: a manifest re-made after a crashed apply supersedes the old journal (kept aside, whole), and
// the dest that journal already copied is re-verified, committed and its source retired — not skipped as migrated.
func TestApplySupersedingFinishesEarlierCopies(t *testing.T) {
	m1, manifest1, j1 := injectAfterTwoCopies(t)
	var copied MigrateItem
	for _, it := range m1.Items {
		if st, ok := j1.Steps[it.ID]; ok && st.State == stepCopied && it.Inline == nil {
			copied = it
			break
		}
	}
	if copied.Source == "" {
		t.Fatalf("no copied file in journal: %+v", j1.Steps)
	}

	m2, manifest2 := inventoryAndWrite(t, InventoryOptions{Now: time.Now().Add(time.Minute)})
	it2, ok := findItem(m2.Items, filepath.Base(copied.Source))
	if !ok || it2.Action != ActionCopy || it2.Dest != copied.Dest {
		t.Fatalf("re-inventoried copied item = %+v, want a copy onto the same dest", it2)
	}

	var out bytes.Buffer
	if _, err := Apply(manifest2, &out); err == nil || !strings.Contains(err.Error(), "incomplete journal") {
		t.Fatalf("plain Apply with another unfinished journal err = %v, want refusal", err)
	}
	res, err := ApplySuperseding(manifest2, &out)
	if err != nil {
		t.Fatalf("ApplySuperseding: %v\n%s", err, out.String())
	}
	if !res.Complete {
		t.Fatalf("superseding apply must complete: %+v\n%s", res, out.String())
	}
	aside := filepath.Join(MigrateStateDir(), filepath.Base(manifest1)+".journal.superseded.json")
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("old journal not kept aside at %s: %v", aside, err)
	}
	assertPathsGone(t, copied.Source, filepath.Join(MigrateStateDir(), filepath.Base(manifest1)+".journal.json"))
	assertStoreCommitCoversImports(t, m2.Items)
	assertAllHashesMatch(t, m2.Items)
	if g := StartupGuard(); !g.Migrated {
		t.Errorf("guard does not see the superseding journal as the completed migration: %+v", g)
	}
}

// TestLegacyArchiveMdGoesUnderGlobalStoreArchive: a *.md directly under LegacyGlobalDir()/archive is a
// global archive item, and its dest lands under the global tier's own archive, not the store root's.
func TestLegacyArchiveMdGoesUnderGlobalStoreArchive(t *testing.T) {
	migrateConfig(t)
	write(t, filepath.Join(LegacyGlobalDir(), "archive", "old.md"), "archived\n")

	m, err := Inventory(InventoryOptions{})
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	it := wantItem(t, m.Items, filepath.Join("archive", "old.md"), KindRepoArchive, ActionCopy)
	want := filepath.Join(StoreDir(), "home", "archive", "old.md")
	if it.Dest != want {
		t.Errorf("legacy global archive dest = %s, want %s", it.Dest, want)
	}
}

// TestMergeAppendOnlyAccessLogs: a grown or diverged access log merges instead of blocking the migration.
func TestMergeAppendOnlyAccessLogs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ dst, src, want string }{
		{"", "a\nb\n", "a\nb\n"},
		{"a\n", "a\nb\nc\n", "a\nb\nc\n"},
		{"a\nb\nc\n", "a\n", "a\nb\nc\n"},
		{"a\nx\n", "a\nb\n", "a\nx\nb\n"},
	} {
		if got := mergeAppendOnly(tc.dst, tc.src); got != tc.want {
			t.Errorf("mergeAppendOnly(%q, %q) = %q, want %q", tc.dst, tc.src, got, tc.want)
		}
	}
}
