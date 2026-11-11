package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// sessionStartContext runs StartSession and returns the context it renders ("" when silent).
func sessionStartContext(t *testing.T, session, cwd string) string {
	t.Helper()
	return StartSession(session, cwd, time.Now())
}

// memoryGitRepo git-inits a fresh repo under a temp dir, skipping the test when git is missing.
func memoryGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestSessionStartAutoInitAndNotice(t *testing.T) {
	repo := memoryGitRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)
	sub := filepath.Join(repo, "sub")
	_ = os.MkdirAll(sub, 0o755)
	// Only UUID-shaped ids advance the chain and resetSeen; probe ids like "probe-x" never clobber it.
	const (
		s0 = "00000000-0000-4000-8000-000000000000"
		s1 = "11111111-1111-4111-8111-111111111111"
		s2 = "22222222-2222-4222-8222-222222222222"
	)
	if got := sessionStartContext(t, s0, sub); got != "" || bankExists(sub) {
		t.Errorf("a repo subdir must not be scaffolded: %q", got)
	}
	// the unified surface is catalog + focus only; seed a catalog so the scaffold notice has a surface to ride on
	if err := os.WriteFile(filepath.Join(StoreDir(), catalogFile), []byte("# Memory catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sessionStartContext(t, s1, repo)
	if !strings.Contains(got, "Memory bank scaffolded at") {
		t.Fatalf("auto-init surface: %q", got)
	}
	_ = writeUpdateResult(s1, UpdateResult{Status: updateFailed, Time: time.Now(), Log: "/x.log"})
	got = sessionStartContext(t, s2, repo)
	if strings.Contains(got, "scaffolded") {
		t.Errorf("notice must not re-announce the scaffold: %q", got[:min(len(got), 500)])
	}
	for _, want := range []string{"Previous session id: " + s1, "did not complete (failed) (log: /x.log)"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q: %q", want, got[:min(len(got), 500)])
		}
	}
	t.Setenv(UpdateChildEnv, "1")
	sessionStartContext(t, "child", repo) // a non-UUID update-child id: excluded by IsUpdateChild regardless of shape
	if e, ok := readChain(filepath.Join(StateDir(), "last-session")); !ok || e != s2 {
		t.Errorf("an update child advanced the session chain: %+v ok=%v", e, ok)
	}
}

// A non-UUID (probe) id never overwrites last-session or seeds a seen-set; a real UUID still does.
func TestSessionStartProbeIDDoesNotClobberLastSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	const real = "33333333-3333-4333-8333-333333333333"
	StartSession(real, repo, time.Now())
	chain := filepath.Join(StateDir(), "last-session")
	if e, ok := readChain(chain); !ok || e != real {
		t.Fatalf("a UUID session id must advance last-session: %+v ok=%v", e, ok)
	}
	StartSession("probe-x", repo, time.Now())
	if e, ok := readChain(chain); !ok || e != real {
		t.Errorf("a probe id must not clobber last-session: got %+v, want %s", e, real)
	}
}

// cutoverStore creates the unified store's git repo, the state memoryContext needs before repairing a native link.
func cutoverStore(t *testing.T) {
	t.Helper()
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// After the cutover StartSession repairs the cwd's native memory link to its repo store dir.
func TestSessionStartLinksNativeMemoryAfterCutover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)

	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	// Pre-scaffold the store (steady state: a .origin exists) so this test isolates the link-repair contract.
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}

	StartSession("s1", repo, time.Now())

	res, ok := linkNativeFor(repo)
	if !ok {
		t.Fatal("linkNativeFor ok=false after cutover")
	}
	if res.Action != "ok" {
		t.Errorf("link action after SessionStart = %s, want ok (already linked)", res.Action)
	}
	fi, err := os.Lstat(res.Link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("SessionStart did not create a native-memory symlink at %s: %v", res.Link, err)
	}
	target, err := os.Readlink(res.Link)
	if err != nil || target != res.Target {
		t.Errorf("link -> %q, want %s (%v)", target, res.Target, err)
	}
}

// A repo's first start after the cutover links native memory to Init's scaffolded store, never a <slug>--<hash> dir.
func TestSessionStartFirstSessionLinksScaffoldedStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	cutoverStore(t)
	repo := filepath.Join(t.TempDir(), "proj")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}

	StartSession("first", repo, time.Now())

	dir := Dir(repo)
	if _, err := readOrigin(dir); err != nil {
		t.Fatalf("store %s has no .origin after the first SessionStart: %v", dir, err)
	}
	if strings.Contains(filepath.Base(dir), "--") {
		t.Errorf("store key got a collision suffix on a fresh repo: %s", dir)
	}
	res, ok := linkNativeFor(repo)
	if !ok || res.Action != "ok" || res.Target != dir {
		t.Errorf("native link after first SessionStart = %+v ok=%v, want ok -> %s", res, ok, dir)
	}
}

// idle backdates tr's mtime so the transcript reads as quiet.
func idle(t *testing.T, path string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// An update worker's own SessionStart never dispatches the catch-up sweep.
func TestSessionStartCatchUpUpdateChildNeverSpawns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(UpdateChildEnv, "")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(repo); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 5)
	idle(t, tr, 20*time.Minute)
	chain := filepath.Join(StateDir(), "last-session")
	writeChain(chain, "prev3")
	var sweeps int
	orig := spawn.catchup
	spawn.catchup = func(string, string) bool { sweeps++; return true }
	t.Cleanup(func() { spawn.catchup = orig })

	t.Setenv(UpdateChildEnv, "1")
	got := StartSession("44444444-4444-4444-8444-444444444445", repo, time.Now())
	if sweeps != 0 {
		t.Errorf("an update child must never spawn the catch-up sweep, calls=%d", sweeps)
	}
	if strings.Contains(got, "Catching up") {
		t.Errorf("an update child's SessionStart must not surface the catch-up note: %q", got)
	}
}

// The session surface carries no all-repos catalog rows, however many repos the catalog lists.
func TestSessionStartCarriesNoCatalogRows(t *testing.T) {
	recallEnv(t)
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n## Repos\n\n")
	for i := range 120 {
		fmt.Fprintf(&cat, "- repo-%03d — some description of the repo · last 2026-10-29 → repo-%03d/MEMORY.md\n", i, i)
	}
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(StoreDir(), catalogFile), []byte(cat.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := StartSession("probe-x", t.TempDir(), time.Now())
	if strings.Contains(got, "repo-000") || strings.Contains(got, "repo-119") {
		t.Errorf("catalog rows injected:\n%s", got)
	}
	if !strings.Contains(got, "other repos:") || len(got) > 1500 {
		t.Errorf("want the one-line pointer and a small surface, got %d bytes:\n%s", len(got), got)
	}
}

// TestStartSessionProbesBankOnce: a banked repo's start probes the bank once for the scaffold and dream checks.
func TestStartSessionProbesBankOnce(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "banked")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	probes := 0
	orig := hasBank
	hasBank = func(r string) bool { probes++; return orig(r) }
	t.Cleanup(func() { hasBank = orig })
	StartSession("00000000-0000-4000-8000-0000000000a0", root, time.Now())
	if probes != 1 {
		t.Fatalf("bank probes per StartSession = %d, want 1", probes)
	}
}

// A corrupt or relative root-<id> file is replaced by the first valid root instead of lingering all session.
func TestRecordSessionRootRepairsCorruptFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwd := canonTemp(t, t.TempDir())
	for _, bad := range []string{"relative/path\n", "\n"} {
		path := sessionRootPath(id)
		write(t, path, bad)
		recordSessionRoot(id, cwd)
		got, err := os.ReadFile(path)
		if err != nil || !strings.HasPrefix(string(got), "/") {
			t.Fatalf("bad %q not repaired: %q, %v", bad, got, err)
		}
		if SessionRoot(id, "fallback") == "fallback" {
			t.Errorf("SessionRoot still falls back after repairing %q", bad)
		}
	}
}

func TestRecordSessionRootReportsLockTimeout(t *testing.T) {
	isolatedConfig(t)
	orig := sessionRootLockWait
	sessionRootLockWait = 200 * time.Millisecond
	t.Cleanup(func() { sessionRootLockWait = orig })
	const id = "11111111-2222-3333-4444-555555555555"
	cwd := canonTemp(t, t.TempDir())
	path := sessionRootPath(id)
	write(t, path+".lock", "")
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() {
		os.Stderr = original
		if err := stderr.Close(); err != nil {
			t.Error(err)
		}
	})
	ran, err := paths.TryFileLock(path+".lock", 0, func() error {
		recordSessionRoot(id, cwd)
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold session-root lock: ran=%v, err=%v", ran, err)
	}
	output, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := "memory-start: record session root: " + paths.ErrLockTimeout.Error() + ": " + path + ".lock\n"
	if string(output) != want {
		t.Errorf("lock timeout diagnostic: got %q, want %q", output, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("session root must remain unwritten while locked: %v", err)
	}
}

// A valid existing root is kept: the first root wins.
func TestRecordSessionRootKeepsValidFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	keep := canonTemp(t, t.TempDir())
	write(t, sessionRootPath(id), keep+"\n")
	recordSessionRoot(id, canonTemp(t, t.TempDir()))
	if got := SessionRoot(id, ""); got != keep {
		t.Errorf("first root replaced: got %q, want %q", got, keep)
	}
}

// Racing repairs of one corrupt root leave one root, and every caller sees that same root once it returns.
func TestRecordSessionRootRacingRepairsAgree(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwds := make([]string, 6)
	for i := range cwds {
		cwds[i] = canonTemp(t, t.TempDir())
	}
	for round := range 40 {
		write(t, sessionRootPath(id), "relative/path\n")
		seen := make([]string, len(cwds))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, cwd := range cwds {
			wg.Go(func() {
				<-start
				recordSessionRoot(id, cwd)
				seen[i] = SessionRoot(id, "")
			})
		}
		close(start)
		wg.Wait()
		final := SessionRoot(id, "")
		for i, got := range seen {
			if got == "" || got != final {
				t.Fatalf("round %d: caller %d saw %q, final root %q", round, i, got, final)
			}
		}
	}
}

// TestSessionRootForSkipsMainProbeOutsideRepo: a cwd git calls no work tree is not probed again for its main checkout.
func TestSessionRootForSkipsMainProbeOutsideRepo(t *testing.T) {
	tempConfig(t)
	dir := canonTemp(t, t.TempDir())
	probes := countGitProbes(t, "--git-common-dir")
	if got := sessionRootFor(dir); got != dir {
		t.Fatalf("sessionRootFor(%s) = %q, want the dir itself", dir, got)
	}
	if *probes != 0 {
		t.Errorf("sessionRootFor ran the main-checkout probe %d times after the toplevel probe failed, want 0", *probes)
	}
}

// The catalog leaves the surface (one pointer stays); the Parked section leaves the injected surface (with a count), the other sections stay, and recall can still find it.
func TestUnifiedSurfaceFocusOnly(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(StoreDir(), catalogFile), "# Memory catalog\n\n- proj — hook · last never -> proj/MEMORY.md\n")
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n"+
		"## Current Focus\n\n- shipping the router\n\n"+
		"## Open Questions\n\n- should not appear\n\n"+
		"## Blockers\n\n- none\n\n"+
		"## Next Steps\n\n- ship it\n\n"+
		"## Parked\n\n- should not appear either\n")
	write(t, filepath.Join(dir, "progress.md"), "# Progress\n\n"+
		"## What Works\n\n- should not appear\n\n"+
		"## In Progress\n\n- the router\n\n"+
		"## What's Next\n\n- should not appear\n")

	s := sessionSurface(repo)
	for _, want := range []string{"other repos:", "## Current Focus", "shipping the router",
		"## Blockers", "## Next Steps", "ship it", "## In Progress", "the router"} {
		if !strings.Contains(s, want) {
			t.Errorf("surface missing %q:\n%s", want, s)
		}
	}
	for _, absent := range []string{"Open Questions", "should not appear", "What Works", "What's Next"} {
		if strings.Contains(s, absent) {
			t.Errorf("surface carried an out-of-scope section (%q):\n%s", absent, s)
		}
	}
	if n := len([]rune(s)); n > totalCapChars {
		t.Errorf("unified surface %d chars > cap %d", n, totalCapChars)
	}
}

// TestUnifiedSurfaceCarriesNoCatalogRows: a 200-repo catalog adds one pointer line and no rows, and the home
// bank's focus rides along with this repo's.
func TestUnifiedSurfaceCarriesNoCatalogRows(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n")
	for i := range 200 {
		fmt.Fprintf(&cat, "- repo-%03d — hook · last never -> repo-%03d/MEMORY.md\n", i, i)
	}
	write(t, filepath.Join(StoreDir(), catalogFile), cat.String())
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- local focus\n")
	write(t, filepath.Join(HomeBankDir(), "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- home essential\n")

	s := sessionSurface(repo)
	if strings.Contains(s, "repo-000") || strings.Contains(s, "repo-199") || strings.Contains(s, "MEMORY CATALOG") {
		t.Errorf("catalog rows leaked into the surface:\n%s", s)
	}
	for _, want := range []string{"other repos:", "local focus", "home essential"} {
		if !strings.Contains(s, want) {
			t.Errorf("surface missing %q:\n%s", want, s)
		}
	}
	if len(s) > 1200 {
		t.Errorf("slim surface is %d bytes, want well under the old 8.5 KB cap", len(s))
	}
}

// TestSectionBodyEdgeCases: case-insensitive heading match, an absent heading, an empty section, and the
// last section in the file all resolve correctly.
func TestSectionBodyEdgeCases(t *testing.T) {
	t.Parallel()
	text := "# Active\n\n## Current Focus\n\n- a\n- b\n\n## Blockers\n\n- none\n"
	if got := sectionBody(text, "Current Focus"); got != "- a\n- b" {
		t.Errorf("sectionBody = %q", got)
	}
	if got := sectionBody(text, "current focus"); got != "- a\n- b" {
		t.Errorf("sectionBody must be case-insensitive, got %q", got)
	}
	if got := sectionBody(text, "Next Steps"); got != "" {
		t.Errorf("an absent heading must yield \"\", got %q", got)
	}
	if got := sectionBody("# Active\n\n## Empty\n\n## Blockers\n\n- x\n", "Empty"); got != "" {
		t.Errorf("an empty section must yield \"\", got %q", got)
	}
	if got := sectionBody(text, "Blockers"); got != "- none" {
		t.Errorf("the last section in the file must still be captured, got %q", got)
	}
}

// Correctness fix (mdsmith AST headings, md-spec.md): a "## " line inside a fenced code block is no
// longer mistaken for the next heading, so the fence's full contents stay inside the section.
func TestSectionBodyRespectsFence(t *testing.T) {
	t.Parallel()
	text := "# Active\n\n## Current Focus\n\n```\n## not a heading\n```\n- a\n\n## Blockers\n\n- none\n"
	want := "```\n## not a heading\n```\n- a"
	if got := sectionBody(text, "Current Focus"); got != want {
		t.Errorf("sectionBody = %q, want %q (fence-aware heading scan must keep the fenced \"## \" line in-section)", got, want)
	}
}

// TestAccessLogPathMovesToStateAfterCutover: store dirs log under state/harness/memory/access, never inside memory/.
func TestAccessLogPathMovesToStateAfterCutover(t *testing.T) {
	isolatedConfig(t)
	legacy := filepath.Join(t.TempDir(), "bank")
	if got := AccessLogPath(legacy); got != filepath.Join(legacy, accessLogFile) {
		t.Fatalf("pre-cutover path = %s, want beside the bank", got)
	}
	initStore(t)
	for dir, want := range map[string]string{
		StoreDir():                           "home.log",
		filepath.Join(StoreDir(), ".claude"): ".claude.log",
		filepath.Join(StoreDir(), "repo-a"):  "repo-a.log",
	} {
		if got := AccessLogPath(dir); got != filepath.Join(AccessStateDir(), want) {
			t.Errorf("AccessLogPath(%s) = %s, want %s", dir, got, want)
		}
	}
	logAccess(filepath.Join(StoreDir(), "repo-a"), time.Now(), "aa11bb22", "read")
	if _, err := os.Stat(filepath.Join(StoreDir(), "repo-a", accessLogFile)); !os.IsNotExist(err) {
		t.Errorf("access.log written inside the store: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(AccessStateDir(), "repo-a.log")); !strings.Contains(string(data), "aa11bb22") {
		t.Errorf("state access log missing the entry: %q", data)
	}
}

// TestKnowledgeIsSearchedAndCataloged: memory/<bank>/knowledge notes (not its README) are search hits and catalog rows.
func TestKnowledgeIsSearchedAndCataloged(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	write(t, filepath.Join(knowledgeDir(HomeBankDir()), "README.md"), "# Knowledge\nzebracorn readme\n")
	write(t, filepath.Join(knowledgeDir(HomeBankDir()), "zebra.md"), "# Zebra\nZebracorn invariant: probes never ship.\n")
	hits := searchBanks(t.TempDir(), "zebracorn invariant probes", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "zebra.md" {
		t.Fatalf("knowledge not ranked first: %+v", hits)
	}
	for _, h := range hits {
		if h.File == "README.md" {
			t.Errorf("the tier README was indexed as knowledge")
		}
	}
	c, err := BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, "## Knowledge") || !strings.Contains(c, "home/knowledge/zebra.md — Zebracorn invariant") || strings.Contains(c, "README.md —") {
		t.Errorf("catalog knowledge section wrong (err=%v):\n%s", err, c)
	}
}

// TestRepoBankKnowledgeIsCatalogedAndSearched: a repo bank's knowledge/*.md shows under its own key in the
// catalog and is reachable by search, same as the home-bank tier.
func TestRepoBankKnowledgeIsCatalogedAndSearched(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := gitRepo(t, t.TempDir(), "narwhal")
	dir, err := EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(knowledgeDir(Dir(root)), "README.md"), "# Knowledge\nnarwhal readme\n")
	write(t, filepath.Join(knowledgeDir(Dir(root)), "narwhal.md"), "# Narwhal\nNarwhalcorn invariant: tusks never ship.\n")
	hits := searchBanks(root, "narwhalcorn invariant tusks", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "narwhal.md" {
		t.Fatalf("repo bank knowledge not ranked first: %+v", hits)
	}
	key := filepath.Base(dir)
	c, err := BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, key+"/knowledge/narwhal.md — Narwhalcorn invariant") || strings.Contains(c, key+"/README.md —") {
		t.Errorf("catalog missing repo bank knowledge row (err=%v):\n%s", err, c)
	}
}

// TestLegacyDocsKnowledgeIgnored: a leftover <ConfigDir>/docs/knowledge tree is no longer searched or cataloged.
func TestLegacyDocsKnowledgeIgnored(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	initStore(t)
	write(t, filepath.Join(cfg, "docs", "knowledge", "yak.md"), "# Yak\nYakcorn invariant: wool never ships.\n")
	if hits := searchBanks(t.TempDir(), "yakcorn invariant wool", 5); len(hits) != 0 {
		t.Errorf("legacy docs/knowledge note was searched: %+v", hits)
	}
	c, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c, "yak.md") {
		t.Errorf("legacy docs/knowledge note leaked into the catalog:\n%s", c)
	}
}

// TestUnifiedSurfaceFallsBackWhenNativeUnlinked: only a missing/blocked native link adds the store's MEMORY.md.
func TestUnifiedSurfaceFallsBackWhenNativeUnlinked(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	repo := canonTemp(t, t.TempDir())
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Skip("git unavailable")
	}
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, nativeIndexFile), "- [Topic](topic.md) — fallback marker\n")
	if s := sessionSurface(repo); !strings.Contains(s, "native link missing") || !strings.Contains(s, "fallback marker") {
		t.Fatalf("unlinked surface lacks the MEMORY.md fallback:\n%s", s)
	}
	if _, ok := linkNativeFor(repo); !ok {
		t.Fatal("linkNativeFor failed")
	}
	if s := sessionSurface(repo); strings.Contains(s, "native link missing") {
		t.Errorf("linked surface still carries the fallback:\n%s", s)
	}
}

func TestSectionBodyBareHeadingDoesNotPanic(t *testing.T) {
	if got := sectionBody("## \nfoo\n## Current Focus\nbar\n", "Current Focus"); got != "bar" {
		t.Errorf("sectionBody = %q, want %q", got, "bar")
	}
}

func TestSessionSurfaceRelativeNativeLink(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := paths.CanonDeep(gitRepo(t, t.TempDir(), "relative-link")) // production keys the link by the resolved path
	dir, err := EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, nativeIndexFile), "unique native-only index marker\n")
	link := projectsMemory(paths.ProjectSlug(root))
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(filepath.Dir(link), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(link, nativeIndexFile)); got != "unique native-only index marker\n" {
		t.Fatalf("relative native link is not readable: %q", got)
	}
	got := sessionSurface(root)
	if strings.Contains(got, "unique native-only index marker") || strings.Contains(got, "native link missing or blocked") {
		t.Fatalf("surface duplicated the index despite a valid relative native link: %q", got)
	}
}
