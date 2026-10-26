package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type aliasEnv struct {
	cfg, a, b, ka, kb string // config dir, checkouts, store keys
	da, db            string // store dirs
}

// newAliasEnv builds an isolated config (itself a decoy git repo), a cut-over store and two repo stores.
func newAliasEnv(t *testing.T) aliasEnv {
	t.Helper()
	cfg, _ := isolatedConfig(t)
	fixtureGit(t, cfg, "init", "-q", "-b", "main")
	fixtureGit(t, cfg, "commit", "-q", "--allow-empty", "-m", "decoy")
	initStore(t)
	parent := canonTemp(t, t.TempDir())
	e := aliasEnv{cfg: cfg, a: gitRepo(t, parent, "alpha"), b: gitRepo(t, parent, "beta")}
	var err error
	if e.da, err = EnsureRepoDir(e.a); err != nil {
		t.Fatal(err)
	}
	if e.db, err = EnsureRepoDir(e.b); err != nil {
		t.Fatal(err)
	}
	e.ka, e.kb = filepath.Base(e.da), filepath.Base(e.db)
	return e
}

func aliasRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// treeHash digests every file and symlink target under dir except .git.
func aliasTreeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			tg, _ := os.Readlink(p)
			lines = append(lines, rel+"->"+tg)
		case d.IsDir():
			lines = append(lines, rel+"/")
		default:
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			lines = append(lines, rel+":"+hex.EncodeToString(s[:]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	h.Write([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}

func alias(t *testing.T, e aliasEnv, dry bool) (AliasResult, error) {
	return AliasRepo(AliasOptions{Repo: e.b, Into: e.ka, DryRun: dry, Now: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)})
}

func TestAliasFoldsSeparateStore(t *testing.T) {
	e := newAliasEnv(t)
	write(t, filepath.Join(e.da, "decisionLog.md"), "# Decisions\n\n## 2026-03-01 — Keep <!-- id:aaaaaaaa -->\nshared body\n\n## 2026-02-01 — TargetOnly <!-- id:bbbbbbbb -->\nt body\n")
	write(t, filepath.Join(e.db, "decisionLog.md"), "# Decisions\n\n## 2026-03-01 — Keep <!-- id:aaaaaaaa -->\nshared body\n\n## 2026-04-01 — SourceOnly <!-- id:cccccccc -->\ns body\n")
	write(t, filepath.Join(e.da, "conventions.md"), "# Conv\n\n- rule one <!-- id:11111111 -->\n")
	write(t, filepath.Join(e.db, "conventions.md"), "# Conv\n\n- rule one <!-- id:11111111 -->\n- rule two <!-- id:22222222 -->\n")
	write(t, filepath.Join(e.da, "sessionHistory.md"), "# Hist\n\n## 2026-04-20 — t <!-- id:33333333 -->\nx\n")
	write(t, filepath.Join(e.db, "sessionHistory.md"), "# Hist\n\n## 2026-04-21 — s <!-- id:44444444 -->\ny\n")
	for _, f := range []string{"activeContext.md", "progress.md", "projectContext.md"} {
		write(t, filepath.Join(e.da, f), "TARGET "+f+"\n")
		write(t, filepath.Join(e.db, f), "SOURCE "+f+"\n")
	}
	write(t, filepath.Join(e.da, "same.md"), "same\n")
	write(t, filepath.Join(e.db, "same.md"), "same\n")
	write(t, filepath.Join(e.db, "topic.md"), "only source topic\n")
	write(t, filepath.Join(e.da, NativeIndexFile), "- [Shared](shared.md) — one\n")
	write(t, filepath.Join(e.db, NativeIndexFile), "- [Shared](shared.md) — one\n- [Extra](extra.md) — two\n")
	write(t, filepath.Join(e.db, "archive", "old.md"), "old\n")
	write(t, filepath.Join(e.db, "handoffs", "h.md"), "handoff\n")
	write(t, filepath.Join(e.db, "reports", "r.md"), "report\n")

	link := filepath.Join(e.cfg, "projects", "-some-proj", "memory")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.db, link); err != nil {
		t.Fatal(err)
	}
	decoyHead := strings.TrimSpace(rawGit(t, e.cfg, "rev-parse", "HEAD"))
	before, _ := StoreRepo().Log(50)

	res, err := alias(t, e, false)
	if err != nil {
		t.Fatalf("AliasRepo: %v", err)
	}
	if res.Key != e.ka || res.Source != e.kb {
		t.Fatalf("result = %+v", res)
	}

	// source gone, catalog row gone, target row stays
	if pathExists(e.db) {
		t.Errorf("source store %s not removed", e.db)
	}
	cat := aliasRead(t, filepath.Join(StoreDir(), CatalogFile))
	if strings.Contains(cat, "- "+e.kb+" ") || !strings.Contains(cat, "- "+e.ka+" ") {
		t.Errorf("catalog rows wrong:\n%s", cat)
	}
	// origin alias recorded and resolves
	o, err := ReadOrigin(e.da)
	if err != nil || !o.Accepts(e.b) {
		t.Fatalf("origin after alias = %+v err=%v, want alias %s", o, err, e.b)
	}
	if got, _ := RepoDir(e.b); got != e.da {
		t.Errorf("RepoDir(beta) = %s, want %s", got, e.da)
	}
	// append-only merge, no duplicates
	dl := aliasRead(t, filepath.Join(e.da, "decisionLog.md"))
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		if n := strings.Count(dl, "id:"+id); n != 1 {
			t.Errorf("decisionLog id %s count = %d\n%s", id, n, dl)
		}
	}
	if strings.Index(dl, "SourceOnly") > strings.Index(dl, "TargetOnly") {
		t.Errorf("decisionLog not newest-first:\n%s", dl)
	}
	cv := aliasRead(t, filepath.Join(e.da, "conventions.md"))
	if strings.Count(cv, "rule one") != 1 || strings.Count(cv, "rule two") != 1 {
		t.Errorf("conventions:\n%s", cv)
	}
	sh := aliasRead(t, filepath.Join(e.da, "sessionHistory.md"))
	if !strings.Contains(sh, "id:33333333") || !strings.Contains(sh, "id:44444444") {
		t.Errorf("sessionHistory:\n%s", sh)
	}
	// replace-type: target wins, source archived
	for _, f := range []string{"activeContext.md", "progress.md", "projectContext.md"} {
		if got := aliasRead(t, filepath.Join(e.da, f)); got != "TARGET "+f+"\n" {
			t.Errorf("%s = %q, target must win", f, got)
		}
		if got := aliasRead(t, filepath.Join(e.da, "archive", "merged-"+e.kb+"-"+f)); got != "SOURCE "+f+"\n" {
			t.Errorf("archived %s = %q", f, got)
		}
	}
	// topics, archive, handoffs, reports copied; equal topic skipped
	for p, want := range map[string]string{"topic.md": "only source topic\n", "archive/old.md": "old\n", "handoffs/h.md": "handoff\n", "reports/r.md": "report\n", "same.md": "same\n"} {
		if got := aliasRead(t, filepath.Join(e.da, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	// native pointer lines appended, deduped
	nv := aliasRead(t, filepath.Join(e.da, NativeIndexFile))
	if strings.Count(nv, "[Shared]") != 1 || strings.Count(nv, "[Extra]") != 1 {
		t.Errorf("MEMORY.md:\n%s", nv)
	}
	// symlink repointed
	if tg, err := filepath.EvalSymlinks(link); err != nil || tg != e.da {
		t.Errorf("symlink -> %q err=%v, want %s", tg, err, e.da)
	}
	// commit landed in the memory repo, not the decoy
	after, _ := StoreRepo().Log(50)
	if len(after) <= len(before) {
		t.Errorf("memory repo gained no commit: before %d after %d", len(before), len(after))
	}
	if dh := strings.TrimSpace(rawGit(t, e.cfg, "rev-parse", "HEAD")); dh != decoyHead {
		t.Errorf("decoy HEAD moved %s -> %s", decoyHead, dh)
	}
	if st, _ := StoreRepo().Status(); len(st) != 0 {
		t.Errorf("memory repo dirty after alias: %v", st)
	}
	// a new commit mentions the fold in its subject
	if !strings.Contains(strings.Join(after[:len(after)-len(before)], "\n"), "alias") && !strings.Contains(strings.Join(after[:len(after)-len(before)], "\n"), "fold") {
		t.Errorf("new commits do not mention the fold: %v", after[:len(after)-len(before)])
	}

	// second run: checkout already resolves to the key -> error
	if _, err := alias(t, e, false); err == nil {
		t.Errorf("second run succeeded; expected error")
	} else if !strings.Contains(err.Error(), "already") {
		t.Errorf("second run error = %v", err)
	}
}

func TestAliasDryRunWritesNothing(t *testing.T) {
	e := newAliasEnv(t)
	write(t, filepath.Join(e.db, "decisionLog.md"), "# D\n\n## 2026-04-01 — S <!-- id:cccccccc -->\nb\n")
	write(t, filepath.Join(e.db, "topic.md"), "t\n")
	link := filepath.Join(e.cfg, "projects", "p", "memory")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(e.db, link); err != nil {
		t.Fatal(err)
	}
	h0 := aliasTreeHash(t, e.cfg)
	g0, _ := StoreRepo().Head()
	res, err := alias(t, e, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != e.kb || len(res.Lines) == 0 {
		t.Errorf("dry-run plan = %+v", res)
	}
	if h1 := aliasTreeHash(t, e.cfg); h0 != h1 {
		t.Errorf("dry-run changed the tree")
	}
	if g1, _ := StoreRepo().Head(); g0 != g1 {
		t.Errorf("dry-run committed")
	}
}

func TestAliasErrors(t *testing.T) {
	e := newAliasEnv(t)
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if _, err := AliasRepo(AliasOptions{Repo: e.b, Into: "nope", Now: now}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown key err = %v", err)
	}
	plain := t.TempDir()
	if _, err := AliasRepo(AliasOptions{Repo: plain, Into: e.ka, Now: now}); err == nil || !strings.Contains(err.Error(), "not a git") {
		t.Errorf("non-git err = %v", err)
	}
	if _, err := AliasRepo(AliasOptions{Repo: e.a, Into: e.ka, Now: now}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("self alias err = %v", err)
	}
}

func TestAliasCollisionRefusesAndLeavesTreeIntact(t *testing.T) {
	e := newAliasEnv(t)
	write(t, filepath.Join(e.da, "topic.md"), "target bytes\n")
	write(t, filepath.Join(e.db, "topic.md"), "different bytes\n")
	h0 := aliasTreeHash(t, e.cfg)
	if _, err := alias(t, e, false); err == nil || !strings.Contains(err.Error(), "different bytes") {
		t.Fatalf("topic collision err = %v", err)
	}
	if h1 := aliasTreeHash(t, e.cfg); h0 != h1 {
		t.Errorf("refused fold changed the tree")
	}

	os.Remove(filepath.Join(e.da, "topic.md"))
	os.Remove(filepath.Join(e.db, "topic.md"))
	write(t, filepath.Join(e.da, "handoffs", "h.md"), "a\n")
	write(t, filepath.Join(e.db, "handoffs", "h.md"), "b\n")
	if _, err := alias(t, e, false); err == nil {
		t.Errorf("handoff collision accepted")
	}
	if !pathExists(e.db) {
		t.Errorf("source removed despite refusal")
	}
}

func TestAliasWithoutSeparateStoreRecordsAliasOnly(t *testing.T) {
	e := newAliasEnv(t)
	os.RemoveAll(e.db) // beta has no store now
	res, err := alias(t, e, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "" {
		t.Errorf("Source = %q, want empty", res.Source)
	}
	o, err := ReadOrigin(e.da)
	if err != nil || !o.Accepts(e.b) {
		t.Errorf("alias not recorded in .origin (origin-only path): %+v err=%v", o, err)
	}
}

func TestAliasSecondRunAfterOriginOnlyIsError(t *testing.T) {
	e := newAliasEnv(t)
	os.RemoveAll(e.db)
	if _, err := alias(t, e, false); err != nil {
		t.Fatal(err)
	}
	if _, err := alias(t, e, false); err == nil {
		t.Errorf("repeat origin-only alias: expected error, got nil")
	}
}
