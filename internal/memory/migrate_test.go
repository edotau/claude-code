package memory

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
