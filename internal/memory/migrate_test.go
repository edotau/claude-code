package memory

import (
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// migrateFixture is a committed legacy store: harness + home + two repo banks, an archive name shared across banks.
func migrateFixture(t *testing.T) (harness string, files map[string]string) {
	t.Helper()
	isolatedConfig(t)
	forgetLayout()
	t.Cleanup(func() { flatForced.Store(0); forgetLayout() })
	initStore(t)
	h := harnessStoreKey()
	files = map[string]string{
		h + "/MEMORY.md": "- [Topic](topic.md) — harness topic\n- [Handoff](handoffs/h1.md) — a handoff\n",
		h + "/topic.md":  "topic body\n",
		h + "/decisionLog.md": "# Log\n\n## 2026-11-16 — old call <!-- id:aaaaaaa1 -->\nbody\n\n" +
			"## 2026-11-16 — new call <!-- id:bbbbbbb2 -->\nSupersedes aaaaaaa1\n",
		h + "/handoffs/h1.md":                     "handoff\n",
		h + "/reports/r.md":                       "see memory/" + h + "/handoffs/h1.md and handoffs/h1.md and `reports/r.md`\n",
		h + "/archive/sessionHistory-2026-10.md":  "## 2026-10-01 — s <!-- id:ccccccc3 -->\n",
		"home/MEMORY.md":                          "- [Home](home-topic.md) — shared\n",
		"home/decisionLog.md":                     "## 2026-11-01 — home call <!-- id:ddddddd4 -->\n",
		"home/knowledge/k.md":                     "k\n",
		"alpha/MEMORY.md":                         "- [A](a.md) — alpha topic\n",
		"alpha/a.md":                              "a body\n",
		"alpha/activeContext.md":                  "alpha focus\n",
		"alpha/archive/sessionHistory-2026-10.md": "## 2026-10-02 — s <!-- id:eeeeeee5 -->\n",
		"beta/MEMORY.md":                          "- [B](b.md) — beta topic\n",
		"beta/progress.md":                        "beta progress\n",
		"beta/b.md":                               "b body\n",
	}
	for rel, body := range files {
		if err := paths.AtomicWrite(filepath.Join(StoreDir(), filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitFixture(t, "fixture")
	return h, files
}

func commitFixture(t *testing.T, msg string) {
	t.Helper()
	if _, err := StoreRepo().run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreRepo().run("commit", "-m", msg); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every tracked store file to its bytes.
func snapshot(t *testing.T) map[string]string {
	t.Helper()
	files, err := listTracked()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := readStoreFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

var anyID = regexp.MustCompile(`id:[0-9a-f]{8}`)

func idSet(snap map[string]string) []string {
	var ids []string
	for _, b := range snap {
		ids = append(ids, anyID.FindAllString(b, -1)...)
	}
	slices.Sort(ids)
	return ids
}

func mustMigrate(t *testing.T, o MigrateOptions) {
	t.Helper()
	if err := Migrate(o); err != nil {
		t.Fatalf("Migrate(%+v): %v", o, err)
	}
}

func TestMigrateApplyKeepsEntryIDs(t *testing.T) {
	h, _ := migrateFixture(t)
	before := snapshot(t)
	mustMigrate(t, MigrateOptions{})
	after := snapshot(t)
	if !slices.Equal(idSet(before), idSet(after)) || len(idSet(after)) != 5 {
		t.Fatalf("entry ids changed: before %v after %v", idSet(before), idSet(after))
	}
	for _, want := range []string{layoutFile, h + "--decisionLog.md", "topic.md", "alpha--a.md", "alpha--activeContext.md",
		"beta--progress.md", "decisionLog.md", "knowledge/k.md", "reports/" + h + "--r.md", "handoffs/" + h + "--h1.md",
		"archive/" + h + "--sessionHistory-2026-10.md", "archive/alpha--sessionHistory-2026-10.md", "MEMORY.md"} {
		if _, ok := after[want]; !ok {
			t.Errorf("missing %s after migrate: %v", want, slices.Sorted(maps.Keys(after)))
		}
	}
	if !strings.Contains(after[h+"--decisionLog.md"], "Supersedes aaaaaaa1") {
		t.Error("Supersedes pair lost")
	}
	if st, _ := StoreRepo().Status(); len(st) != 0 {
		t.Errorf("store dirty after migrate: %v", st)
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), "alpha")); !os.IsNotExist(err) {
		t.Errorf("legacy alpha dir survived: %v", err)
	}
}

func TestMigrateRewritesLinksAndMergesIndex(t *testing.T) {
	h, _ := migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	after := snapshot(t)
	if got := after["reports/"+h+"--r.md"]; !strings.Contains(got, "memory/handoffs/"+h+"--h1.md") ||
		!strings.Contains(got, " handoffs/"+h+"--h1.md") || !strings.Contains(got, "`reports/"+h+"--r.md`") {
		t.Errorf("links not rewritten: %q", got)
	}
	idx := after["MEMORY.md"]
	for _, want := range []string{"](topic.md)", "](handoffs/" + h + "--h1.md)", "](home-topic.md)", "## alpha", "](alpha--a.md)", "## beta", "](beta--b.md)"} {
		if !strings.Contains(idx, want) {
			t.Errorf("merged MEMORY.md lacks %q:\n%s", want, idx)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	head, _ := StoreRepo().head()
	var out strings.Builder
	mustMigrate(t, MigrateOptions{Out: &out})
	if !strings.Contains(out.String(), "already flat") {
		t.Errorf("rerun output %q, want already flat", out.String())
	}
	if again, _ := StoreRepo().head(); again != head {
		t.Errorf("rerun committed: %s -> %s", head, again)
	}
}

func TestMigrateRevertRestoresTree(t *testing.T) {
	_, _ = migrateFixture(t)
	if err := Index(io.Discard, io.Discard, IndexOptions{Repo: paths.ConfigDir()}); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t)
	slug := filepath.Join(paths.ConfigDir(), "projects", "p1", "memory")
	if err := os.MkdirAll(filepath.Dir(slug), 0o755); err != nil {
		t.Fatal(err)
	}
	oldTarget := filepath.Join(StoreDir(), "alpha")
	if err := os.Symlink(oldTarget, slug); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	mustMigrate(t, MigrateOptions{})
	if got, _ := os.Readlink(slug); got != StoreDir() {
		t.Errorf("link -> %s, want the store root %s", got, StoreDir())
	}
	mustMigrate(t, MigrateOptions{Revert: true})
	got := snapshot(t)
	for rel, w := range want {
		if got[rel] != w {
			t.Errorf("%s after revert = %q, want %q", rel, got[rel], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("revert has %d files, fixture %d", len(got), len(want))
	}
	if link, _ := os.Readlink(slug); link != oldTarget {
		t.Errorf("link -> %s, want %s", link, oldTarget)
	}
}

func TestMigrateRevertKeepsPostMigrationFiles(t *testing.T) {
	h, _ := migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	if err := paths.AtomicWrite(filepath.Join(StoreDir(), "reports", "alpha--new.md"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := paths.AtomicWrite(filepath.Join(StoreDir(), h+"--conventions.md"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, "post-migration writes")
	mustMigrate(t, MigrateOptions{Revert: true})
	got := snapshot(t)
	for _, want := range []string{"alpha/reports/new.md", h + "/conventions.md"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s not restored by the prefix rule: %v", want, slices.Sorted(maps.Keys(got)))
		}
	}
}

func TestMigrateRefusesLiveLock(t *testing.T) {
	migrateFixture(t)
	want := snapshot(t)
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	ran, lerr := paths.TryFileLock(BankLockPath(), time.Second, func() error {
		err = Migrate(MigrateOptions{})
		return nil
	})
	if lerr != nil || !ran {
		t.Fatalf("could not hold the bank lock: ran=%v err=%v", ran, lerr)
	}
	if !errors.Is(err, ErrMigrateRefused) || !strings.Contains(err.Error(), "held memory lock") {
		t.Fatalf("Migrate under a held lock = %v, want a named refusal", err)
	}
	if !maps.Equal(snapshot(t), want) || stampPresent() {
		t.Error("a refused migrate changed the store")
	}
}

func TestMigrateRefusesDirtyStore(t *testing.T) {
	migrateFixture(t)
	if err := os.WriteFile(filepath.Join(StoreDir(), "alpha", "a.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Migrate(MigrateOptions{})
	if !errors.Is(err, ErrMigrateRefused) || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("Migrate on a dirty store = %v, want a dirty refusal", err)
	}
}

func TestMigrateRefusesCollision(t *testing.T) {
	migrateFixture(t)
	for rel, body := range map[string]string{"home/reports/alpha--x.md": "h\n", "alpha/reports/x.md": "a\n"} {
		if err := paths.AtomicWrite(filepath.Join(StoreDir(), filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commitFixture(t, "collision")
	err := Migrate(MigrateOptions{})
	if !errors.Is(err, ErrMigrateRefused) || !strings.Contains(err.Error(), "reports/alpha--x.md") {
		t.Fatalf("Migrate with a target collision = %v, want a refusal naming it", err)
	}
}

func TestMigrateRefusesCredentialShapedFileBeforeApply(t *testing.T) {
	migrateFixture(t)
	key := "tvly-" + strings.Repeat("a", 20) // built at runtime so no literal key sits in the source
	if err := paths.AtomicWrite(filepath.Join(StoreDir(), "alpha", "reports", "leak.md"), []byte("k="+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, "leak")
	err := Migrate(MigrateOptions{})
	if !errors.Is(err, ErrMigrateRefused) || !strings.Contains(err.Error(), "reports/alpha--leak.md") {
		t.Fatalf("Migrate with a credential-shaped file = %v, want a refusal naming it", err)
	}
	if stampPresent() {
		t.Fatal("a refused migration must not stamp the store")
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	migrateFixture(t)
	want := snapshot(t)
	var out strings.Builder
	mustMigrate(t, MigrateOptions{DryRun: true, Format: "json", Out: &out})
	if !strings.Contains(out.String(), `"files_after"`) || !strings.Contains(out.String(), `"to": "alpha--a.md"`) {
		t.Errorf("dry-run json lacks the plan: %s", out.String())
	}
	if !maps.Equal(snapshot(t), want) || stampPresent() {
		t.Error("dry run changed the store")
	}
}

func TestMigrateDryRunBlockedExitsOne(t *testing.T) {
	migrateFixture(t)
	if err := os.WriteFile(filepath.Join(StoreDir(), "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(MigrateOptions{DryRun: true}); !errors.Is(err, ErrMigrateBlocked) {
		t.Fatalf("dry run with a dirty store = %v, want ErrMigrateBlocked", err)
	}
}

func TestFlatTarget(t *testing.T) {
	isolatedConfig(t)
	h := harnessStoreKey()
	for _, tc := range []struct{ dir, rel, want string }{
		{"alpha", "activeContext.md", "alpha--activeContext.md"},
		{h, "decisionLog.md", h + "--decisionLog.md"},
		{h, "topic.md", "topic.md"},
		{"alpha", "topic.md", "alpha--topic.md"},
		{homeDir, "decisionLog.md", "decisionLog.md"},
		{"alpha", "reports/x.md", "reports/alpha--x.md"},
		{"alpha", "archive/dream-backup/y.md", "archive/alpha--dream-backup/y.md"},
		{homeDir, "knowledge/k.md", "knowledge/k.md"},
		{"alpha", "other/x.md", ""},
		{"alpha", "notes.txt", ""},
		{"alpha", "reports/", ""},
	} {
		got, ok := flatTarget(tc.dir, tc.rel)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("flatTarget(%s, %s) = %q,%v want %q", tc.dir, tc.rel, got, ok, tc.want)
		}
	}
}

func TestRewriteLinks(t *testing.T) {
	isolatedConfig(t)
	dirs := []string{"alpha", "beta", homeDir}
	got, n := rewriteLinks("a ~/.claude/memory/alpha/reports/x.md, (reports/y.md) archive/z.md. activeContext.md /x/memory/home/knowledge/k.md",
		"beta", dirs)
	want := "a ~/.claude/memory/reports/alpha--x.md, (reports/beta--y.md) archive/beta--z.md. activeContext.md /x/memory/knowledge/k.md"
	if got != want || n != 4 {
		t.Errorf("rewriteLinks = %q (%d), want %q (4)", got, n, want)
	}
	if got, n := rewriteLinks("xmemory/alpha/reports/x.md", "", dirs); n != 0 || got != "xmemory/alpha/reports/x.md" {
		t.Errorf("a glued token was rewritten: %q", got)
	}
}

func migrateWrite(t *testing.T, rel, text string) {
	t.Helper()
	if err := paths.AtomicWrite(filepath.Join(StoreDir(), filepath.FromSlash(rel)), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateCatalogRoundTrip(t *testing.T) {
	migrateFixture(t)
	migrateWrite(t, catalogFile, "catalog\n")
	commitFixture(t, "catalog")
	if err := Index(io.Discard, io.Discard, IndexOptions{Repo: paths.ConfigDir()}); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t)
	mustMigrate(t, MigrateOptions{})
	mustMigrate(t, MigrateOptions{Revert: true})
	if !maps.Equal(snapshot(t), want) {
		t.Fatal("catalog moved or original tree changed")
	}
	mustMigrate(t, MigrateOptions{})
}

func TestMigrateRevertCollisionNoLoss(t *testing.T) {
	h, _ := migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	migrateWrite(t, h+"--topic.md", "second topic\n")
	commitFixture(t, "conflicting topic")
	want := snapshot(t)
	if err := Migrate(MigrateOptions{Revert: true}); !errors.Is(err, ErrMigrateRefused) {
		t.Fatalf("revert collision = %v, want refusal", err)
	}
	if !maps.Equal(snapshot(t), want) {
		t.Fatal("collision changed the tree")
	}
}

func TestRewriteLinksLeavesCodeAndFlatPaths(t *testing.T) {
	isolatedConfig(t)
	text := "archive/tar waves/run.go:294 reports/alpha--x.md memory/alpha/reports/alpha--x.md"
	if got, n := rewriteLinks(text, "alpha", []string{"alpha"}); got != text || n != 0 {
		t.Fatalf("rewrote code/flat paths: %q (%d)", got, n)
	}
}

func TestMigrateEditedLinksRoundTrip(t *testing.T) {
	h, files := migrateFixture(t)
	rel := h + "/reports/r.md"
	files[rel] += "memory/home/knowledge/k.md memory/" + h + "/topic.md\n"
	migrateWrite(t, rel, files[rel])
	commitFixture(t, "links")
	mustMigrate(t, MigrateOptions{})
	flat := "reports/" + h + "--r.md"
	cur, err := readStoreFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	migrateWrite(t, flat, string(cur)+"\nuser edit\n")
	commitFixture(t, "edited report")
	mustMigrate(t, MigrateOptions{Revert: true})
	if got := snapshot(t)[rel]; got != files[rel]+"\nuser edit\n" {
		t.Fatalf("edited report links = %q, want %q", got, files[rel]+"\nuser edit\n")
	}
}

func TestMigrateIndexNonPointerText(t *testing.T) {
	migrateFixture(t)
	text := "# Alpha\n<!-- retained -->\nwrapped description\n- [A](a.md)\n  continued\n"
	migrateWrite(t, "alpha/MEMORY.md", text)
	migrateWrite(t, "home/MEMORY.md", "# Home\nHome note\n")
	commitFixture(t, "index prose")
	mustMigrate(t, MigrateOptions{})
	idx := snapshot(t)[nativeIndexFile]
	for _, want := range []string{"## alpha\n# Alpha\n<!-- retained -->\nwrapped description", "  continued", "## home\n# Home\nHome note"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index lost %q: %s", want, idx)
		}
	}
	mustMigrate(t, MigrateOptions{Revert: true})
	if snapshot(t)["alpha/MEMORY.md"] != text {
		t.Fatal("index revert changed original")
	}
}

func TestMigrateNoIndexesRoundTrip(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	migrateWrite(t, "alpha/a.md", "a\n")
	migrateWrite(t, "alpha/progress.md", "p\n")
	commitFixture(t, "without indexes")
	want := snapshot(t)
	mustMigrate(t, MigrateOptions{})
	mustMigrate(t, MigrateOptions{Revert: true})
	got := snapshot(t)
	for rel, body := range want {
		if got[rel] != body {
			t.Fatalf("changed %s", rel)
		}
	}
	if !strings.Contains(got[catalogFile], "# Memory catalog") || !strings.Contains(got["home/MEMORY.md"], bankRegionBegin) {
		t.Fatal("missing refreshed indexes")
	}
	t.Cleanup(func() { flatForced.Store(0); forgetLayout() })
}

func TestMigrateIndexExactLineMatch(t *testing.T) {
	migrateFixture(t)
	migrateWrite(t, "alpha/MEMORY.md", "- [A](a.md) extended\n")
	commitFixture(t, "long pointer")
	mustMigrate(t, MigrateOptions{})
	cur := snapshot(t)[nativeIndexFile]
	migrateWrite(t, nativeIndexFile, cur+"\n## alpha\n- [A](alpha--a.md)\n")
	commitFixture(t, "short pointer")
	mustMigrate(t, MigrateOptions{Revert: true})
	if got := snapshot(t)["alpha/MEMORY.md"]; got != "- [A](a.md) extended\n- [A](a.md)\n" {
		t.Fatalf("new shorter pointer lost: %q", got)
	}
}

func TestMigrateRefusesPendingJournal(t *testing.T) {
	migrateFixture(t)
	if err := paths.AtomicWrite(filepath.Join(JournalDir(), "pending.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(MigrateOptions{}); !errors.Is(err, ErrMigrateRefused) {
		t.Fatalf("pending journal = %v, want refusal", err)
	}
}

func TestMigrateDottedWatcher(t *testing.T) {
	migrateFixture(t)
	if err := paths.AtomicWrite(watchClaimPath("session.with.dot"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(MigrateOptions{}); !errors.Is(err, ErrMigrateRefused) {
		t.Fatalf("live dotted watcher = %v, want refusal", err)
	}
}

func TestMigrateRevertRestoresForce(t *testing.T) {
	migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	flatForced.Store(0)
	mustMigrate(t, MigrateOptions{Revert: true, DryRun: true})
	if got := flatForced.Load(); got != 0 {
		t.Fatalf("force leaked: %d", got)
	}
}

func TestMigrateRefusesAmbiguousNames(t *testing.T) {
	p := planMigrate([]string{"home/reports/alpha--x.md"}, "claude-code")
	if len(p.Unplanned) != 1 {
		t.Fatalf("ambiguous name accepted: %+v", p.migReport)
	}
}

func TestMigrateResumesStampedCommit(t *testing.T) {
	h, _ := migrateFixture(t)
	link := filepath.Join(paths.ConfigDir(), "projects", "resume", "memory")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(StoreDir(), h)
	if err := os.Symlink(old, link); err != nil {
		t.Skip(err)
	}
	p, err := planFlat(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pre, err := StoreRepo().head()
	if err != nil {
		t.Fatal(err)
	}
	rec := migRecord{Pre: pre, Harness: h, Dirs: p.dirs, IndexDirs: p.indexDirs, Moves: p.Moves, Links: []migLink{{Link: link, Old: old}}}
	if _, err := writeRevertRecord(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := commitChangesLocked(flatSubject, flatChanges(p)); err != nil {
		t.Fatal(err)
	}
	mustMigrate(t, MigrateOptions{})
	if got, err := os.Readlink(link); err != nil || got != StoreDir() {
		t.Fatalf("resume link = %s, %v", got, err)
	}
	mustMigrate(t, MigrateOptions{Revert: true})
	if got, err := os.Readlink(link); err != nil || got != old {
		t.Fatalf("revert link = %s, %v", got, err)
	}
}

func TestMigrateRevertLinkErrorRetry(t *testing.T) {
	migrateFixture(t)
	link := filepath.Join(paths.ConfigDir(), "projects", "blocked", "memory")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(StoreDir(), "alpha")
	if err := os.Symlink(old, link); err != nil {
		t.Skip(err)
	}
	mustMigrate(t, MigrateOptions{})
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(MigrateOptions{Revert: true}); err == nil {
		t.Fatal("revert swallowed link replacement error")
	}
	if err := Migrate(MigrateOptions{}); !errors.Is(err, ErrMigrateRefused) {
		t.Fatalf("apply during unfinished revert = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustMigrate(t, MigrateOptions{Revert: true})
	if got, err := os.Readlink(link); err != nil || got != old {
		t.Fatalf("retry link = %s, %v", got, err)
	}
}

func TestMigrateEditedMixedLinksRoundTrip(t *testing.T) {
	h, _ := migrateFixture(t)
	original := "old handoffs/h1.md already flat handoffs/" + h + "--h1.md\n"
	migrateWrite(t, h+"/topic.md", original)
	commitFixture(t, "mixed legacy and flat links")
	mustMigrate(t, MigrateOptions{})
	migrateWrite(t, "topic.md", snapshot(t)["topic.md"]+"\nuser edit\n")
	commitFixture(t, "edited mixed links")
	mustMigrate(t, MigrateOptions{Revert: true})
	if got := snapshot(t)[h+"/topic.md"]; got != original+"\nuser edit\n" {
		t.Fatalf("mixed links changed: %q", got)
	}
}

func TestMigrateRetainsUnchangedTargetCollision(t *testing.T) {
	p := planMigrate([]string{"alpha/a.md", "alpha--a.md"}, "claude-code")
	if len(p.Collisions) != 1 {
		t.Fatalf("root collision missed: %+v", p.migReport)
	}
}

func TestMigrateRevertRefusesIndexProseLoss(t *testing.T) {
	migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	migrateWrite(t, nativeIndexFile, snapshot(t)[nativeIndexFile]+"new prose must survive\n")
	commitFixture(t, "index prose edit")
	want := snapshot(t)
	if err := Migrate(MigrateOptions{Revert: true}); !errors.Is(err, ErrMigrateRefused) {
		t.Fatalf("index prose edit = %v, want safe refusal", err)
	}
	if !maps.Equal(snapshot(t), want) {
		t.Fatal("index prose refusal changed tree")
	}
}

func TestMigrateRefreshesCatalogAfterFlip(t *testing.T) {
	h, _ := migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	catalog := aliasRead(t, filepath.Join(StoreDir(), catalogFile))
	if !strings.Contains(catalog, "alpha--projectContext.md") {
		t.Fatalf("stale flat catalog: %s", catalog)
	}
	mustMigrate(t, MigrateOptions{Revert: true})
	catalog = aliasRead(t, filepath.Join(StoreDir(), catalogFile))
	if !strings.Contains(catalog, h+"/MEMORY.md") || strings.Contains(catalog, "alpha--projectContext.md") {
		t.Fatalf("stale legacy catalog: %s", catalog)
	}
}

func TestPlanMigrateBlocksBankWithoutTypedFile(t *testing.T) {
	p := planMigrate([]string{"alpha/a.md", "beta/progress.md", "beta/b.md"}, "claude-code")
	if len(p.Blockers) != 1 || !strings.HasPrefix(p.Blockers[0], "alpha has no typed file") {
		t.Fatalf("Blockers = %v, want only alpha", p.Blockers)
	}
}

func TestMigrateRevertKeepsUnsectionedIndexPointers(t *testing.T) {
	h, _ := migrateFixture(t)
	for _, d := range []string{h, homeDir, "beta"} {
		if err := os.Remove(filepath.Join(StoreDir(), d, nativeIndexFile)); err != nil {
			t.Fatal(err)
		}
	}
	commitFixture(t, "only alpha has an index")
	mustMigrate(t, MigrateOptions{})
	migrateWrite(t, nativeIndexFile, "- [New](topic.md) — new root pointer\n"+snapshot(t)[nativeIndexFile])
	commitFixture(t, "root pointer")
	mustMigrate(t, MigrateOptions{Revert: true})
	if got := snapshot(t)[h+"/"+nativeIndexFile]; !strings.Contains(got, "- [New](topic.md) — new root pointer") {
		t.Fatalf("unsectioned pointer lost: %q", got)
	}
}

func TestMigrateRevertUsesCurrentIndex(t *testing.T) {
	h, _ := migrateFixture(t)
	mustMigrate(t, MigrateOptions{})
	cur := snapshot(t)[nativeIndexFile]
	cur = strings.ReplaceAll(cur, "- [A](alpha--a.md) — alpha topic\n", "")
	cur = strings.ReplaceAll(cur, "- [Topic](topic.md) — harness topic\n", "")
	cur = strings.ReplaceAll(cur, "- [Home](home-topic.md) — shared\n", "")
	cur = strings.ReplaceAll(cur, "- [B](beta--b.md) — beta topic", "- [Renamed](beta--b.md) — current fact")
	migrateWrite(t, nativeIndexFile, cur)
	commitFixture(t, "delete and edit index facts")
	mustMigrate(t, MigrateOptions{Revert: true})
	got := snapshot(t)
	for _, d := range []string{h, homeDir, "alpha", "beta"} {
		text := got[d+"/"+nativeIndexFile]
		for _, stale := range []string{"harness topic", "alpha topic", "beta topic", "— shared"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s resurrected %q: %s", d, stale, text)
			}
		}
	}
	if !strings.Contains(got["beta/MEMORY.md"], "- [Renamed](b.md) — current fact") {
		t.Fatalf("edited fact lost: %q", got["beta/MEMORY.md"])
	}
}

func TestMigrateRelativeMarkdownLinks(t *testing.T) {
	migrateFixture(t)
	original := "[self](a.md) [archive](./archive/y.md#fact) [other](../beta/b.md)\n" +
		"[url](https://example.com/a.md) [anchor](#fact) [missing](archive/missing.md)\n" +
		"```md\n[self](a.md) [archive](archive/y.md)\n```\n" +
		"~~~markdown\n[other](../beta/b.md)\n~~~\n"
	migrateWrite(t, "alpha/a.md", original)
	migrateWrite(t, "alpha/archive/y.md", "[back](../a.md)\n")
	commitFixture(t, "relative links")
	mustMigrate(t, MigrateOptions{})
	got := snapshot(t)
	want := strings.Replace(original, "[self](a.md) [archive](./archive/y.md#fact) [other](../beta/b.md)",
		"[self](alpha--a.md) [archive](archive/alpha--y.md#fact) [other](beta--b.md)", 1)
	if got["alpha--a.md"] != want || got["archive/alpha--y.md"] != "[back](../alpha--a.md)\n" {
		t.Errorf("relative links = %q, nested = %q; want %q", got["alpha--a.md"], got["archive/alpha--y.md"], want)
	}
	migrateWrite(t, "alpha--a.md", got["alpha--a.md"]+"\nuser edit\n")
	commitFixture(t, "edited relative links")
	mustMigrate(t, MigrateOptions{Revert: true})
	if got := snapshot(t)["alpha/a.md"]; got != original+"\nuser edit\n" {
		t.Fatalf("edited relative links did not round trip: %q", got)
	}
}

func TestMigrateLegacyCollisionKey(t *testing.T) {
	migrateFixture(t)
	const key = "legacy---a1b2"
	for rel, text := range map[string]string{
		key + "/progress.md":  "see memory/" + key + "/archive/y.md\n",
		key + "/MEMORY.md":    "- [Y](archive/y.md)\n",
		key + "/archive/y.md": "[progress](../progress.md)\n",
		".origins":            "origin record\n",
	} {
		migrateWrite(t, rel, text)
	}
	commitFixture(t, "legacy collision key")
	if err := Index(io.Discard, io.Discard, IndexOptions{Repo: paths.ConfigDir()}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t)
	mustMigrate(t, MigrateOptions{})
	got := snapshot(t)
	if got["legacy-a1b2--progress.md"] != "see memory/archive/legacy-a1b2--y.md\n" ||
		got["archive/legacy-a1b2--y.md"] != "[progress](../legacy-a1b2--progress.md)\n" ||
		!strings.Contains(got[nativeIndexFile], "## legacy-a1b2\n- [Y](archive/legacy-a1b2--y.md)") ||
		got[".origins"] != before[".origins"] {
		t.Fatalf("legacy key mapping failed: %v", got)
	}
	migrateWrite(t, "legacy-a1b2--new.md", "new\n")
	migrateWrite(t, "legacy-a1b2--progress.md", got["legacy-a1b2--progress.md"]+"user edit\n")
	commitFixture(t, "new file for mapped bank")
	mustMigrate(t, MigrateOptions{Revert: true})
	got = snapshot(t)
	for rel, text := range before {
		if rel == key+"/progress.md" {
			text += "user edit\n"
		}
		if got[rel] != text {
			t.Errorf("%s = %q, want %q", rel, got[rel], text)
		}
	}
	if got[key+"/new.md"] != "new\n" {
		t.Fatal("new file did not revert to the legacy key")
	}
}
