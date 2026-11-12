package pack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMain moves TMPDIR out of /tmp: a stray /tmp/.git makes collect's fail-closed check reject every fixture.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("/var/tmp", "pack-test-")
	if err != nil && runtime.GOOS == "windows" { // no /var/tmp; %TEMP% has no shared /tmp/.git hazard
		root, err = os.MkdirTemp("", "pack-test-")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pack TestMain:", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", root)
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

// write creates path (and its parent dirs) with body, mode 0644.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCollect: globs + walks, skips ignored dirs, binary, and honors caps + truncation.
func TestCollect(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, content string) { write(t, filepath.Join(dir, rel), content) }
	w("a.go", "package a")
	w("sub/b.go", "package b")
	w("node_modules/pkg/index.js", "ignored")
	w("bin.bin", "\x00\x01binary")
	w("big.txt", strings.Repeat("x", 100))

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 10, testBudget)
	if err != nil {
		t.Fatal(err)
	}

	included := map[string]IncludedFile{}
	for _, f := range ctx.Included {
		included[f.Path] = f
	}
	if _, ok := included["a.go"]; !ok {
		t.Error("a.go should be included")
	}
	if _, ok := included["sub/b.go"]; !ok {
		t.Error("sub/b.go should be included (recursive walk)")
	}
	checkCollectSkips(t, ctx)
	// big.txt exceeds maxFileBytes=10 → truncated, original Bytes preserved.
	if f, ok := included["big.txt"]; !ok {
		t.Error("big.txt should be included")
	} else if !f.Truncated || len(f.Content) != 10 || f.Bytes != 100 {
		t.Errorf("big.txt truncation wrong: truncated=%t len=%d bytes=%d", f.Truncated, len(f.Content), f.Bytes)
	}
}

// checkCollectSkips requires the pruned-dir and binary skip reasons from TestCollect's tree.
func checkCollectSkips(t *testing.T, ctx Context) {
	t.Helper()
	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	// A pruned directory is not a file: neither it nor anything inside it appears in Skipped.
	for _, p := range []string{"node_modules", "node_modules/pkg/index.js"} {
		if r, ok := skipReasons[p]; ok {
			t.Errorf("%s inside a pruned dir must not be counted as a skipped file, got %q", p, r)
		}
	}
	// bin.bin has a NUL byte → binary-content.
	if r := skipReasons["bin.bin"]; r != "binary-content" {
		t.Errorf("bin.bin should be binary-content, got %q", r)
	}
}

// TestCollectMaxFiles: max-files cap marks the overflow skipped.
func TestCollectMaxFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, filepath.Join(dir, n), "x")
	}
	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 2, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 2 {
		t.Errorf("included = %d, want 2 (cap)", len(ctx.Included))
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Reason == "max-files-exceeded" {
			found = true
		}
	}
	if !found {
		t.Error("expected a max-files-exceeded skip")
	}
}

// TestCollectGlobError: a malformed --pattern is recorded as Skipped, not silently dropped.
func TestCollectGlobError(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "x")

	// "[" is an unterminated character class: filepath.Glob returns ErrBadPattern for it.
	ctx, err := Collect(t.Context(), dir, nil, []string{"["}, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Path == "[" && strings.HasPrefix(s.Reason, "glob-error: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a glob-error Skipped entry for pattern %q, got %+v", "[", ctx.Skipped)
	}
}

// TestCollectWalkError: an unreadable subdir is recorded as Skipped, not vanished silently.
func TestCollectWalkError(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("root and Windows ignore directory permission bits")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	if err := os.Mkdir(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) }) // let t.TempDir() clean up

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Path == "locked" && strings.HasPrefix(s.Reason, "walk-error: ") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a walk-error Skipped entry for %q, got %+v", "locked", ctx.Skipped)
	}
}

// TestCollectSkipsSecretLike: secret-like paths are always skipped, even outside a git repo.
func TestCollectSkipsSecretLike(t *testing.T) {
	dir := t.TempDir()
	w := func(rel, content string) { write(t, filepath.Join(dir, rel), content) }
	w("a.go", "package a")
	w(".env", "SECRET=1")
	w(".env.local", "SECRET=2")
	w("secrets.env", "SECRET=3")
	w("k.pem", "cert")
	w("k.key", "key")
	w("env.d/x.env", "x")
	w("state/y.txt", "y")

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	included := map[string]bool{}
	for _, inc := range ctx.Included {
		included[inc.Path] = true
	}
	if !included["a.go"] || !included["env.d/x.env"] || len(ctx.Included) != 2 {
		t.Errorf("Included = %+v, want a.go and env.d/x.env (env.d is no longer secret-like)", ctx.Included)
	}
	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	for _, p := range []string{".env", ".env.local", "secrets.env", "k.pem", "k.key", "state/y.txt"} {
		if skipReasons[p] != "secret-like" {
			t.Errorf("%s reason = %q, want secret-like", p, skipReasons[p])
		}
	}
}

// Inside a git work tree, gitignored files are skipped while tracked and untracked files are included.
func TestCollectHonoursGitIgnore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n.gitignore\n")
	write(t, filepath.Join(dir, "u.go"), "package u")
	write(t, filepath.Join(dir, "ignored.txt"), "ignore me")

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "a.go"},
		{"commit", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	var included []string
	for _, f := range ctx.Included {
		included = append(included, f.Path)
	}
	sort.Strings(included)
	if !reflect.DeepEqual(included, []string{"a.go", "u.go"}) {
		t.Errorf("Included = %v, want [a.go u.go]", included)
	}
	skipReasons := map[string]string{}
	for _, s := range ctx.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	if skipReasons["ignored.txt"] != "gitignored" {
		t.Errorf("ignored.txt reason = %q, want gitignored", skipReasons["ignored.txt"])
	}
}

// TestCollectFailsClosedOnLsFilesError: a corrupt index must error, not walk gitignored files.
func TestCollectFailsClosedOnLsFilesError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".gitignore"), ".env\n")
	write(t, filepath.Join(dir, ".env"), "SECRET=1")
	write(t, filepath.Join(dir, "a.go"), "package a")
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	write(t, filepath.Join(dir, ".git", "index"), "not a valid index")

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
	if err == nil {
		t.Fatalf("err = nil, want ls-files error; Included = %v", ctx.Included)
	}
	if !strings.Contains(err.Error(), "git ls-files") {
		t.Errorf("err = %v, want it to name git ls-files", err)
	}
}

// TestCollectAbsDir: an absolute --dirs entry resolves independent of cwd.
func TestCollectAbsDir(t *testing.T) {
	target := t.TempDir()
	write(t, filepath.Join(target, "a.go"), "package a")

	other := t.TempDir()
	ctx, err := Collect(t.Context(), other, []string{target}, nil, 40, 100, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range ctx.Included {
		if strings.HasSuffix(f.Path, "a.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a.go under absolute dir %s to be found, got %+v", target, ctx.Included)
	}
}

// TestCollectBoundedRead: a large file is bounded-read: original size preserved, content capped.
func TestCollectBoundedRead(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 1<<20))

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 1024, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 1 {
		t.Fatalf("Included = %d, want 1", len(ctx.Included))
	}
	f := ctx.Included[0]
	if f.Bytes != 1<<20 {
		t.Errorf("Bytes = %d, want %d", f.Bytes, 1<<20)
	}
	if !f.Truncated {
		t.Error("expected Truncated = true")
	}
	if len(f.Content) > 1024 {
		t.Errorf("len(Content) = %d, want <= 1024", len(f.Content))
	}
}

// Many files over testBudget are capped at the byte budget (not the file cap); overflow is "budget-exceeded".
func TestCollectByteBudget(t *testing.T) {
	dir := t.TempDir()
	const perFile = 100_000
	const numFiles = 35 // 3.5 MB total, over testBudget
	for i := range numFiles {
		write(t, filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), strings.Repeat("x", perFile))
	}

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 1000, 1<<20, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, f := range ctx.Included {
		total += len(f.Content)
	}
	if total > testBudget {
		t.Errorf("inlined bytes = %d, want <= testBudget (%d)", total, testBudget)
	}
	if len(ctx.Included)+len(ctx.Skipped) != numFiles {
		t.Fatalf("included+skipped = %d, want %d (every file accounted for)", len(ctx.Included)+len(ctx.Skipped), numFiles)
	}
	found := false
	for _, s := range ctx.Skipped {
		if s.Reason == "budget-exceeded" {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one budget-exceeded skip once the total exceeds testBudget")
	}
}

// Combined size comfortably under the byte budget skips nothing for budget reasons.
func TestCollectByteBudgetUnderLimit(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, filepath.Join(dir, n), strings.Repeat("y", 100))
	}

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 1024, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Included) != 3 {
		t.Fatalf("included = %d, want 3 (well under the byte budget)", len(ctx.Included))
	}
	for _, s := range ctx.Skipped {
		if s.Reason == "budget-exceeded" {
			t.Errorf("unexpected budget-exceeded skip for %s: nothing should be cut this far under budget", s.Path)
		}
	}
}

// A tight budget fitting one of two same-size files keeps the non-test source over the alphabetically first _test.go.
func TestCollectOrdersSourceBeforeTests(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("x", 100)
	write(t, filepath.Join(dir, "a_test.go"), body) // alphabetically first, but must rank after source
	write(t, filepath.Join(dir, "z.go"), body)
	write(t, filepath.Join(dir, "testdata", "fixture.txt"), body)

	ctx, err := Collect(t.Context(), dir, []string{"."}, nil, 1000, 1024, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, f := range ctx.Included {
		order = append(order, f.Path)
	}
	wantBefore := func(a, b string) {
		ia, ib := slices.Index(order, a), slices.Index(order, b)
		if ia < 0 || ib < 0 {
			t.Fatalf("order = %v, missing %q or %q", order, a, b)
		}
		if ia >= ib {
			t.Errorf("order = %v, want %q before %q", order, a, b)
		}
	}
	wantBefore("z.go", "a_test.go")
	wantBefore("a_test.go", "testdata/fixture.txt")
}

// testBudget is the old default total (2.5 MiB) for tests that never meant to hit it.
const testBudget = 2560 * 1024

func TestCanceledContext(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, dirs := range [][]string{nil, {"."}} {
		got, err := Collect(ctx, dir, dirs, []string{"*.go"}, 40, 100, testBudget)
		if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
			t.Errorf("Collect(%v) = %+v, %v, want cancellation without files", dirs, got, err)
		}
	}
	if _, err := changedFiles(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedFiles error = %v, want cancellation", err)
	}
	if _, err := changedOrListed(ctx, Spec{Diff: true}, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("changedOrListed error = %v, want cancellation", err)
	}
}

type cancelCase struct {
	name  string
	stage string
	run   func(context.Context, string) (Context, error)
}

func collectRun(ctx context.Context, dir string) (Context, error) {
	return Collect(ctx, dir, []string{"."}, nil, 40, 100, testBudget)
}

func changedRun(ctx context.Context, dir string) (Context, error) {
	_, err := changedFiles(ctx, dir)
	return Context{}, err
}

func changedOrListedRun(ctx context.Context, dir string) (Context, error) {
	_, err := changedOrListed(ctx, Spec{Diff: true}, dir)
	return Context{}, err
}

// fakeGitScript is a git stand-in that touches $PACK_TEST_GIT_MARKER and sleeps when run at stage.
func fakeGitScript(sleep, stage string) string {
	return "#!/bin/sh\n" +
		"if [ \"$3\" = \"" + stage + "\" ]; then\n" +
		"  : > \"$PACK_TEST_GIT_MARKER\"\n" +
		"  exec \"" + sleep + "\" 3\n" +
		"fi\n" +
		"case \"$3\" in\n" +
		"  rev-parse) printf 'true\\n' ;;\n" +
		"  diff) printf 'changed.go\\n' ;;\n" +
		"  ls-files) printf 'tracked.go\\000' ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
}

type cancelResult struct {
	context Context
	err     error
}

// waitForMarker blocks until marker exists, failing if the run returns or 2s pass first.
func waitForMarker(t *testing.T, marker string, done <-chan cancelResult) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case <-tick.C:
		case got := <-done:
			t.Fatalf("git returned before synchronization: %+v, %v", got.context, got.err)
		case <-deadline.C:
			t.Fatal("git did not reach the cancellation synchronization marker")
		}
	}
}

func runCancellationCase(t *testing.T, sleep string, tc cancelCase) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "git-started")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(dir, "ignored.txt"), "must not be inlined")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGitScript(sleep, tc.stage)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PACK_TEST_GIT_MARKER", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan cancelResult, 1)
	go func() {
		got, err := tc.run(ctx, dir)
		done <- cancelResult{got, err}
	}()
	waitForMarker(t, marker, done)
	canceledAt := time.Now()
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("error = %v, want cancellation", got.err)
		}
		if len(got.context.Included) != 0 {
			t.Errorf("canceled scan included files: %+v", got.context.Included)
		}
		if elapsed := time.Since(canceledAt); elapsed > time.Second {
			t.Errorf("git took %s to stop after cancellation", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("git command did not stop after cancellation")
	}
}

func TestGitCommandCancellation(t *testing.T) {
	skipUnlessUnix(t) // the fake git is an sh script
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	for _, tc := range []cancelCase{
		{"collect probe", "rev-parse", collectRun},
		{"collect list", "ls-files", collectRun},
		{"changed diff", "diff", changedRun},
		{"changed list", "ls-files", changedRun},
		{"diff-only spec", "diff", changedOrListedRun},
	} {
		t.Run(tc.name, func(t *testing.T) { runCancellationCase(t, sleep, tc) })
	}
}

// skipUnlessUnix stands in for the //go:build unix constraint these tests' old files carried.
func skipUnlessUnix(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
	default:
		t.Skip("unix only")
	}
}

// fakeGit puts a `git` on PATH that prints stderr and exits with code.
func fakeGit(t *testing.T, code, stderr string) {
	t.Helper()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\necho '"+stderr+"' >&2\nexit "+code+"\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A git failure inside a real repo fails closed; only git's "not a git repository" means non-repo.
func TestCollectGitFailureFailsClosed(t *testing.T) {
	skipUnlessUnix(t)
	repo := t.TempDir()
	write(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(repo, "a.txt"), "hello")
	fakeGit(t, "1", "boom: index corrupt")
	got, err := Collect(context.Background(), repo, []string{"."}, nil, -1, 0, testBudget)
	if err == nil || len(got.Included) != 0 {
		t.Fatalf("want error and no files, got err=%v files=%v", err, got.Included)
	}

	plain := t.TempDir()
	write(t, filepath.Join(plain, "a.txt"), "hello")
	fakeGit(t, "128", "fatal: not a git repository")
	got, err = Collect(context.Background(), plain, []string{"."}, nil, -1, 0, testBudget)
	if err != nil || len(got.Included) != 1 {
		t.Fatalf("non-repo: want 1 file and no error, got err=%v files=%v", err, got.Included)
	}
}
