package worktree

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// originClone is a repo whose origin/HEAD is written as loose refs one commit behind HEAD, without a git clone.
func originClone(t *testing.T) string {
	t.Helper()
	main := gitRepo(t)
	sha := readRef(t, main, "refs/heads/main")
	hosttest.WriteRel(t, main, ".git/refs/remotes/origin/main", sha+"\n")
	hosttest.WriteRel(t, main, ".git/refs/remotes/origin/HEAD", "ref: refs/remotes/origin/main\n")
	commitIn(t, main, "commit", "--allow-empty", "-q", "-m", "unpushed")
	return main
}

// readRef reads a loose ref straight from .git, sparing a rev-parse spawn on the fresh template repo.
func readRef(t *testing.T, main, ref string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(main, ".git", filepath.FromSlash(ref)))
	if err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return strings.TrimSpace(string(data))
}

// rootFor gives main an isolated worktree root, like testRepo but for a caller-built main checkout.
func rootFor(t *testing.T, main string) *Repo {
	t.Helper()
	root := paths.Canon(t.TempDir())
	return &Repo{Main: main, Root: filepath.Join(root, "worktrees", filepath.Base(main))}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func writeBaseRef(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"worktree":{"baseRef":"`+value+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBaseRefFresh(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "freshbase", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	got := revParse(t, dest, "HEAD")
	if want := revParse(t, main, "refs/remotes/origin/HEAD"); got != want {
		t.Fatalf("fresh worktree HEAD = %s, want origin/HEAD %s (create warnings: %s)", got, want, log.String())
	}
	if local := revParse(t, main, "HEAD"); got == local {
		t.Fatal("fresh worktree HEAD equals the unpushed local HEAD; want the origin/HEAD commit")
	}
}

func TestCreateBaseRefHead(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "head")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "headbase", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := revParse(t, dest, "HEAD"), revParse(t, main, "HEAD"); got != want {
		t.Fatalf("head worktree HEAD = %s, want local HEAD %s (create warnings: %s)", got, want, log.String())
	}
}

func TestCreateBaseRefLocalSettingsOverride(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")
	// The main checkout's settings.local.json is read last, so it overrides the user config file.
	writeBaseRef(t, filepath.Join(main, ".claude", "settings.local.json"), "head")

	var log bytes.Buffer
	dest, err := Create(rootFor(t, main), "override", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := revParse(t, dest, "HEAD"), revParse(t, main, "HEAD"); got != want {
		t.Fatalf("override worktree HEAD = %s, want local HEAD %s (create warnings: %s)", got, want, log.String())
	}
}

// Regression for --no-track: a fresh branch has no upstream, so `git branch -d` succeeds after a local merge.
func TestCleanFreshBranchDeletedAfterMerge(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	main := originClone(t)
	writeBaseRef(t, filepath.Join(cfg, "settings.json"), "fresh")
	repo := rootFor(t, main)

	var log bytes.Buffer
	dest, err := Create(repo, "gone", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "work to merge")
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-gone")

	result, err := Clean(repo, "gone", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed {
		t.Fatalf("want removal, got refusal: %s", result.Reason)
	}
	if !result.BranchDeleted {
		t.Error("want the merged fresh-based branch deleted; a branch with an origin upstream survives `branch -d` until pushed")
	}
}

func TestSeedSymlinksVenvCopiesEnv(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Parallel()
	repo := testRepo(t)
	main := repo.Main
	// .gitignore must be committed so the worktree checkout itself ignores the seeded paths.
	if err := os.WriteFile(filepath.Join(main, ".gitignore"), []byte(".venv\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitIn(t, main, "add", ".gitignore")
	commitIn(t, main, "commit", "-q", "-m", "ignore seeded paths")
	if err := os.MkdirAll(filepath.Join(main, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".venv", "bin", "activate"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(main, ".env"), 0o640); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	dest, err := Create(repo, "seeded", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("seed warnings: %s", log.String())
	}

	requireSeededVenv(t, dest, main)
	requireSeededEnv(t, dest)

	cmd := exec.Command("git", "-C", dest, "status", "--porcelain")
	cmd.Env = gitEnv()
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worktree status = %q, err %v; seeded gitignored paths must leave the worktree clean", out, err)
	}
}

// requireSeededVenv fails unless dest/.venv is a symlink to main's absolute .venv.
func requireSeededVenv(t *testing.T, dest, main string) {
	t.Helper()
	info, err := os.Lstat(filepath.Join(dest, ".venv"))
	if err != nil {
		t.Fatalf(".venv missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".venv mode = %v, want a symlink", info.Mode())
	}
	if link, err := os.Readlink(filepath.Join(dest, ".venv")); err != nil || link != filepath.Join(main, ".venv") {
		t.Fatalf(".venv -> %q, err %v; want absolute %q", link, err, filepath.Join(main, ".venv"))
	}
}

// requireSeededEnv fails unless dest/.env is a regular copy with the source's 0640 mode and content.
func requireSeededEnv(t *testing.T, dest string) {
	t.Helper()
	envInfo, err := os.Stat(filepath.Join(dest, ".env"))
	if err != nil {
		t.Fatalf(".env missing: %v", err)
	}
	if !envInfo.Mode().IsRegular() {
		t.Fatalf(".env mode = %v, want a regular file copy", envInfo.Mode())
	}
	if runtime.GOOS != "windows" && envInfo.Mode().Perm() != 0o640 {
		t.Fatalf(".env mode = %o, want 640 (source mode preserved)", envInfo.Mode().Perm())
	}
	if data, err := os.ReadFile(filepath.Join(dest, ".env")); err != nil || string(data) != "TOKEN=secret\n" {
		t.Fatalf(".env content = %q, err %v", data, err)
	}
}

func TestSeedSkipsNonIgnoredEnv(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo.Main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "noseed", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env seeded (stat err = %v) even though it is not gitignored; a seeded tracked path would dirty the worktree", err)
	}
}

// TestSeedChecksDestIgnoreRules: a reused branch whose .gitignore lacks .env gets no copy, though main ignores it.
func TestSeedChecksDestIgnoreRules(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	// predates the ignore rule
	commitIn(t, repo.Main, "branch", "old-branch")
	if err := os.WriteFile(filepath.Join(repo.Main, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commitIn(t, repo.Main, "add", ".gitignore")
	commitIn(t, repo.Main, "commit", "-q", "-m", "ignore .env")
	if err := os.WriteFile(filepath.Join(repo.Main, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "reused", CreateOptions{Branch: "old-branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env copied into a branch that does not ignore it (stat err = %v)", err)
	}
}

// A directory-only `.venv/` ignore rule must still let seedPath symlink .venv into a worktree that has none yet.
func TestSeedPathVenvWithDirOnlyIgnoreRule(t *testing.T) {
	main, dest := t.TempDir(), gitRepo(t)
	if err := os.MkdirAll(filepath.Join(main, ".venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := seedPath(main, dest, ".venv", io.Discard); err != nil {
		t.Fatal(err)
	}
	requireSeededVenv(t, dest, main)
}

// A venv the repo does not ignore is still linked, and git's info/exclude keeps the lane clean.
func TestSeedLinksUnignoredVenvAndExcludesIt(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	t.Parallel()
	repo := testRepo(t)
	if err := os.MkdirAll(filepath.Join(repo.Main, "venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "plainvenv", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "venv")); err != nil || link != filepath.Join(repo.Main, "venv") {
		t.Fatalf("venv -> %q, err %v; want a link to the primary venv", link, err)
	}
	cmd := exec.Command("git", "-C", dest, "status", "--porcelain")
	cmd.Env = gitEnv()
	if out, err := cmd.Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("status = %q, err %v; the venv link must stay invisible to git", out, err)
	}
}

const fakeEditableFinder = `import importlib.abc, importlib.util, sys
MAPPING = {'pkg': '%s'}
class _F(importlib.abc.MetaPathFinder):
    def find_spec(self, name, path=None, target=None):
        p = MAPPING.get(name)
        return p and importlib.util.spec_from_file_location(name, p + "/__init__.py", submodule_search_locations=[p])
def install():
    sys.meta_path.append(_F())
`

// A lane venv reuses the primary packages yet imports the editable package from the lane, leaving the primary alone.
func TestSeedBuildsLaneVenvRerootingEditable(t *testing.T) {
	hosttest.SkipOnWindows(t, "venv layout")
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	main, dest := t.TempDir(), gitRepo(t)
	for dir, v := range map[string]string{main: "1", dest: "2"} {
		mustWriteFile(t, filepath.Join(dir, "src", "pkg", "__init__.py"), "VALUE = "+v+"\n")
	}
	if out, err := exec.Command(py, "-m", "venv", "--without-pip", filepath.Join(main, ".venv")).CombinedOutput(); err != nil {
		t.Skipf("venv unavailable: %v %s", err, out)
	}
	sp := sitePackages(filepath.Join(main, ".venv"))
	mustWriteFile(t, filepath.Join(sp, "__editable___pkg_0_finder.py"), fmt.Sprintf(fakeEditableFinder, filepath.Join(main, "src", "pkg")))
	mustWriteFile(t, filepath.Join(sp, "__editable__.pkg-0.pth"), "import __editable___pkg_0_finder; __editable___pkg_0_finder.install()\n")
	if err := seedPath(main, dest, ".venv", io.Discard); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(dest, ".venv")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf(".venv must be a built venv, got %v, err %v", info, err)
	}
	out, err := exec.Command(filepath.Join(dest, ".venv", "bin", "python"), "-c", "import pkg;print(pkg.VALUE, pkg.__file__)").CombinedOutput()
	if want := "2 "; err != nil || !strings.HasPrefix(string(out), want) || !strings.Contains(string(out), filepath.Join(filepath.Base(dest), "src", "pkg")) {
		t.Fatalf("lane import = %q, err %v; want it from %s", out, err, want)
	}
}

func mustWriteFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseMappingReadsMultilineMixedQuotes(t *testing.T) {
	src := "MAPPING: dict[str, str] = {\n  'a': '/x/a',\n  \"it's\": \"/y/{b}\",\n}\nNAMESPACES = {'ns': ['/z']}\n"
	got := parseMapping(src)
	if len(got) != 2 || got["a"] != "/x/a" || got["it's"] != "/y/{b}" {
		t.Fatalf("parseMapping = %v", got)
	}
}

const nspkgRow = `import sys, types, os;has_mfs = sys.version_info > (3, 5);p = os.path.join(sys._getframe(1).f_locals['sitedir'], *('nspk',));importlib = has_mfs and __import__('importlib.util');has_mfs and __import__('importlib.machinery');m = has_mfs and sys.modules.setdefault('nspk', importlib.util.module_from_spec(importlib.machinery.PathFinder.find_spec('nspk', [os.path.dirname(p)])));m = m or sys.modules.setdefault('nspk', types.ModuleType('nspk'));mp = (m or []) and m.__dict__.setdefault('__path__',[]);(p not in mp) and mp.append(p)
`

// addPrimaryExtras gives the primary venv a dotted-name editable, a namespace .pth and a console script.
func addPrimaryExtras(t *testing.T, main, dest string) {
	t.Helper()
	venv := filepath.Join(main, ".venv")
	sp := sitePackages(venv)
	for dir, v := range map[string]string{main: "1", dest: "2"} {
		mustWriteFile(t, filepath.Join(dir, "src", "ns", "sub", "__init__.py"), "VALUE = "+v+"\n")
	}
	mustWriteFile(t, filepath.Join(sp, "__editable___ns_0_finder.py"),
		strings.Replace(fmt.Sprintf(fakeEditableFinder, filepath.Join(main, "src", "ns", "sub")), "'pkg'", "'ns.sub'", 1))
	mustWriteFile(t, filepath.Join(sp, "__editable__.ns-0.pth"), "import __editable___ns_0_finder; __editable___ns_0_finder.install()\n")
	mustWriteFile(t, filepath.Join(sp, "nspk", "mod.py"), "X = 7\n")
	mustWriteFile(t, filepath.Join(sp, "x-nspkg.pth"), nspkgRow)
	mustWriteFile(t, filepath.Join(sp, "y-nspkg.pth"), strings.ReplaceAll(nspkgRow, "nspk", "ns"))
	script := "#!" + filepath.Join(venv, "bin", "python") + "\nimport pkg\nprint('script', pkg.VALUE)\n"
	mustWriteFile(t, filepath.Join(venv, "bin", "tool"), script)
	mustWriteFile(t, filepath.Join(venv, "bin", "envtool"), "#!/usr/bin/env python\nimport pkg\nprint('env', pkg.VALUE)\n")
	mustWriteFile(t, filepath.Join(sp, "ns", "__init__.py"), "SHADOW = True\n")
	for _, name := range []string{"tool", "envtool"} {
		if err := os.Chmod(filepath.Join(venv, "bin", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeedLaneVenvDottedNamespaceAndScripts(t *testing.T) {
	hosttest.SkipOnWindows(t, "venv layout")
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	main, dest := t.TempDir(), gitRepo(t)
	for dir, v := range map[string]string{main: "1", dest: "2"} {
		mustWriteFile(t, filepath.Join(dir, "src", "pkg", "__init__.py"), "VALUE = "+v+"\n")
	}
	if out, err := exec.Command(py, "-m", "venv", "--without-pip", filepath.Join(main, ".venv")).CombinedOutput(); err != nil {
		t.Skipf("venv unavailable: %v %s", err, out)
	}
	sp := sitePackages(filepath.Join(main, ".venv"))
	mustWriteFile(t, filepath.Join(sp, "__editable___pkg_0_finder.py"), fmt.Sprintf(fakeEditableFinder, filepath.Join(main, "src", "pkg")))
	mustWriteFile(t, filepath.Join(sp, "__editable__.pkg-0.pth"), "import __editable___pkg_0_finder; __editable___pkg_0_finder.install()\n")
	addPrimaryExtras(t, main, dest)
	if err := seedPath(main, dest, ".venv", io.Discard); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if got := run(filepath.Join(dest, ".venv", "bin", "python"), "-c", "import ns.sub, nspk.mod;print(ns.sub.VALUE, ns.sub.__file__, nspk.mod.X)"); !strings.HasPrefix(got, "2 ") || !strings.Contains(got, filepath.Base(dest)) || !strings.HasSuffix(strings.TrimSpace(got), " 7") {
		t.Fatalf("dotted or namespace import = %q", got)
	}
	requireLaneScripts(t, dest, run)
}

// requireLaneScripts runs the primary's scripts from the lane; only a rewritten shebang finds envtool's lane venv.
func requireLaneScripts(t *testing.T, dest string, run func(...string) string) {
	t.Helper()
	for tool, want := range map[string]string{"tool": "script 2", "envtool": "env 2"} {
		if got := run(filepath.Join(dest, ".venv", "bin", tool)); strings.TrimSpace(got) != want {
			t.Fatalf("lane script %s = %q, want %q", tool, got, want)
		}
	}
}

func TestCopyScriptRewritesEnvPythonShebang(t *testing.T) {
	dir := t.TempDir()
	rep := strings.NewReplacer()
	for head, want := range map[string]string{
		"#!/usr/bin/env python3.12\n":    "#!/l/bin/python\n",
		"#!/usr/bin/env -S python3 -u\n": "#!/usr/bin/env -S /l/bin/python -u\n",
		"#!/usr/bin/env python2\n":       "#!/usr/bin/env python2\n",
	} {
		src, dst := filepath.Join(dir, "s"), filepath.Join(dir, "d")
		mustWriteFile(t, src, head+"x\n")
		if err := copyScript(src, dst, "/l/bin/python", rep); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(dst); string(got) != want+"x\n" {
			t.Fatalf("%q -> %q, want %q", head, got, want)
		}
		_ = os.Remove(dst)
	}
}

func TestLaneShebangKeepsHeadForUnsafePath(t *testing.T) {
	if got := laneShebang("#!/usr/bin/env python\n", "/a$b/bin/python"); got != "#!/usr/bin/env python\n" {
		t.Fatalf("spaced path rewrote the shebang: %q", got)
	}
}
