package ask

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
)

// initRepo creates a git repo at t.TempDir() with one commit on main;
func initRepo(t *testing.T) string {
	t.Helper()
	dir := hosttest.Repo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestParseScope(t *testing.T) {
	cases := []struct {
		name                   string
		uncommitted            bool
		base, commit           string
		pr                     bool
		wantKind, wantRef, err string
	}{
		{name: "none defaults to uncommitted", wantKind: "uncommitted"},
		{name: "uncommitted alone", uncommitted: true, wantKind: "uncommitted"},
		{name: "base alone", base: "main", wantKind: "base", wantRef: "main"},
		{name: "commit alone", commit: "abc123", wantKind: "commit", wantRef: "abc123"},
		{name: "pr alone", pr: true, wantKind: "pr"},
		{name: "two set errors", uncommitted: true, base: "main", err: "at most one"},
		{name: "base and commit both set errors", base: "main", commit: "abc", err: "at most one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := parseScope(c.uncommitted, c.base, c.commit, c.pr)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want containing %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if s.Kind != c.wantKind || s.Ref != c.wantRef {
				t.Errorf("parseScope = %+v, want Kind=%q Ref=%q", s, c.wantKind, c.wantRef)
			}
		})
	}
}

func TestDiffUncommitted(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := diff(context.Background(), dir, diffScope{Kind: "uncommitted"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "two") {
		t.Errorf("diff missing the HEAD-relative edit:\n%s", s)
	}
	if !strings.Contains(s, "new file: new.txt") || !strings.Contains(s, "untracked content") {
		t.Errorf("diff missing the untracked file block:\n%s", s)
	}
}

func TestDiffUntrackedQuotedNames(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "config", "core.quotePath", "true")
	for _, name := range []string{"tab\tname.txt", "double\"quote.txt", "café.txt"} {
		t.Run(name, func(t *testing.T) {
			const content = "untracked review content\n"
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := diff(context.Background(), dir, diffScope{Kind: "uncommitted"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "new file: "+name+"\n"+content) {
				t.Fatalf("untracked file %q vanished from review:\n%s", name, out)
			}
		})
	}
	t.Run("vanished file is reported", func(t *testing.T) {
		var out strings.Builder
		if n := appendUntrackedWithin(&out, dir, "vanished.txt", untrackedCap); n != 0 {
			t.Fatalf("missing file contributed %d content bytes", n)
		}
		if !strings.Contains(out.String(), "new file: vanished.txt (unreadable, skipped:") {
			t.Fatalf("Lstat error silently dropped: %q", out.String())
		}
	})
}

func TestLastDispatchErrorBeyondOtherAgentsRecentJobs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const want = "split review part 1: provider unavailable"
	records := []agent.Record{
		{Agent: "codex", Status: "error", Error: "older failure"},
		{Agent: "codex", Status: "error", Error: want},
		{Agent: "gemini", Status: "error", Error: "unrelated failure"},
		{Agent: "copilot", Status: "ok"},
		{Agent: "gemini", Status: "ok"},
		{Agent: "copilot", Status: "ok"},
		{Agent: "gemini", Status: "ok"},
		{Agent: "codex", Status: "ok"},
	}
	start := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	for i, fixture := range records {
		rec := agent.NewRecord(fixture.Agent, agent.RunOptions{}, start.Add(time.Duration(i)*time.Second))
		rec.Status, rec.Error = fixture.Status, fixture.Error
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
	}
	if got := lastDispatchError("codex"); got != want {
		t.Fatalf("newest failed codex dispatch hidden by newer jobs: got %q, want %q", got, want)
	}
}

func TestDiffUncommittedNothingInScope(t *testing.T) {
	dir := initRepo(t)
	if _, err := diff(context.Background(), dir, diffScope{Kind: "uncommitted"}); err == nil || err.Error() != "nothing in scope" {
		t.Fatalf("err = %v, want \"nothing in scope\"", err)
	}
}

func TestDiffBase(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nfeature line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "feature work")

	out, err := diff(context.Background(), dir, diffScope{Kind: "base", Ref: "main"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "feature line") {
		t.Errorf("base diff missing the feature commit:\n%s", out)
	}
}

func TestDiffCommit(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "second")
	sha := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))

	out, err := diff(context.Background(), dir, diffScope{Kind: "commit", Ref: sha})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "two") {
		t.Errorf("commit diff missing the change:\n%s", out)
	}
}

func TestDiffMergeCommitUsesFirstParent(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "switch", "-q", "-c", "feature")
	commitGitFile(t, dir, "one\nmerged-feature-line\n", "feature")
	runGit(t, dir, "switch", "-q", "main")
	runGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	sha := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	if parents := strings.Fields(gitOut(t, dir, "show", "-s", "--format=%P", sha)); len(parents) != 2 {
		t.Fatalf("fixture must be a merge commit, got parents %v", parents)
	}
	if combined := gitOut(t, dir, "show", "--no-color", "--format=", sha); strings.TrimSpace(combined) != "" {
		t.Fatalf("fixture must have an empty default combined diff, got %q", combined)
	}

	out, err := diff(context.Background(), dir, diffScope{Kind: "commit", Ref: sha})
	if err != nil {
		t.Fatalf("clean merge commit must remain in review scope: %v", err)
	}
	if !strings.Contains(string(out), "+merged-feature-line") {
		t.Fatalf("merge review missing the first-parent change:\n%s", out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// pr scope resolves origin/HEAD without a real remote by faking the ref directly.
func TestDiffPR(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/HEAD", "refs/heads/main")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\npr line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "pr work")

	out, err := diff(context.Background(), dir, diffScope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "pr line") {
		t.Errorf("pr diff missing the feature commit:\n%s", out)
	}
}

func TestReviewPromptAdversarialVerdictMarker(t *testing.T) {
	p := reviewPrompt(true, "")
	if !strings.Contains(p, "VERDICT: no actionable findings") {
		t.Errorf("adversarial prompt missing its verdict marker:\n%s", p)
	}
}

func TestReviewPromptNeutralAsksForVerdictFirst(t *testing.T) {
	p := reviewPrompt(false, "")
	if !strings.Contains(p, "SHIP IT") || !strings.Contains(p, "NEEDS WORK") || !strings.Contains(p, "BLOCK") {
		t.Errorf("neutral prompt missing verdict vocabulary:\n%s", p)
	}
}

func TestReviewPromptFocusAppended(t *testing.T) {
	p := reviewPrompt(true, "the auth module")
	if !strings.HasSuffix(p, "Focus: the auth module") {
		t.Errorf("focus not appended:\n%s", p)
	}
}

// reviewArgs's temp file must survive for a detached background run, and be cleaned up immediately
// for a foreground run whose lifetime ends with this call.
func TestReviewArgsCleanupLifecycle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, background := range []bool{false, true} {
		args, cleanup, err := reviewArgs("codex", diffScope{Kind: "uncommitted"}, []byte("diff content"), true, background, "", "", "")
		if err != nil {
			t.Fatalf("reviewArgs(background=%v): %v", background, err)
		}
		tmp := reviewAttachPath(t, args)
		data, err := os.ReadFile(tmp)
		if err != nil || string(data) != "diff content" {
			t.Fatalf("temp file content = %q, err %v, want %q", data, err, "diff content")
		}
		cleanup()
		_, statErr := os.Stat(tmp)
		if background {
			if statErr != nil {
				t.Errorf("background cleanup removed the attach file it must leave for the detached child: %v", statErr)
			}
			os.Remove(tmp) // test cleanup only
		} else {
			if statErr == nil {
				t.Error("foreground cleanup left the temp attach file behind")
			}
		}
	}
}

func reviewAttachPath(t *testing.T, args []string) string {
	t.Helper()
	idx := -1
	for i, a := range args {
		if a == "--attach" {
			idx = i + 1
		}
	}
	if idx < 0 || idx >= len(args) {
		t.Fatalf("args = %v, missing --attach", args)
	}
	return args[idx]
}

func TestReviewArgsShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	args, cleanup, err := reviewArgs("codex", diffScope{Kind: "uncommitted"}, []byte("d"), false, false, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	assertreviewArgsPrefix(t, args)
	if args[len(args)-1] != reviewPrompt(false, "") {
		t.Errorf("last arg = %q, want the review prompt", args[len(args)-1])
	}
	for _, a := range args {
		if a == "--background" {
			t.Error("foreground run must not carry --background")
		}
	}
}

func assertreviewArgsPrefix(t *testing.T, args []string) {
	t.Helper()
	if len(args) < 6 || args[0] != "--agent" || args[1] != "codex" || args[2] != "--fallback" || args[3] != "agents" || args[4] != "--attach" {
		t.Fatalf("args = %v, want [--agent codex --fallback agents --attach <tmp> <prompt>]", args)
	}
}

func TestReviewArgsBackgroundFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	args, cleanup, err := reviewArgs("codex", diffScope{Kind: "uncommitted"}, []byte("d"), false, true, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, a := range args {
			if strings.HasSuffix(a, ".diff") {
				os.Remove(a)
			}
		}
	})
	_ = cleanup
	found := false
	for _, a := range args {
		if a == "--background" {
			found = true
		}
	}
	if !found {
		t.Errorf("args = %v, missing --background", args)
	}
}

func TestReviewArgsRubricPrecedesPrompt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Given a rubric file; When args are built; Then "--rubric <f>" sits right before the trailing prompt
	args, cleanup, err := reviewArgs("codex", diffScope{Kind: "uncommitted"}, []byte("d"), false, false, "", "/tmp/rubric.md", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	n := len(args)
	if n < 3 || args[n-3] != "--rubric" || args[n-2] != "/tmp/rubric.md" || args[n-1] != reviewPrompt(false, "") {
		t.Errorf("args tail = %v, want [--rubric /tmp/rubric.md <prompt>]", args[max(0, n-3):])
	}
}

func TestReviewArgsEmptyRubricEmitsNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	args, cleanup, err := reviewArgs("codex", diffScope{Kind: "uncommitted"}, []byte("d"), false, false, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if slices.Contains(args, "--rubric") {
		t.Errorf("args = %v, want no --rubric", args)
	}
}

func TestReadCappedBoundsLargeFile(t *testing.T) {
	// Given a file larger than untrackedCap; When read; Then exactly untrackedCap bytes come back
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), untrackedCap+100), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readCapped(path)
	if err != nil || len(got) != untrackedCap {
		t.Fatalf("readCapped = %d bytes, err %v, want %d", len(got), err, untrackedCap)
	}
}

func TestDiffInlinesUntrackedFileCapped(t *testing.T) {
	// Given an oversized untracked file; When Diff runs; Then it succeeds and inlines at most the cap
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), bytes.Repeat([]byte("z"), untrackedCap+100), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := diff(context.Background(), dir, diffScope{Kind: "uncommitted"})
	if err != nil {
		t.Fatal(err)
	}
	if c := bytes.Count(out, []byte("z")); c != untrackedCap {
		t.Errorf("inlined %d bytes of the untracked file, want %d", c, untrackedCap)
	}
}

func TestParseScopeRejectsDashRef(t *testing.T) {
	for _, c := range []struct{ base, commit string }{{"-x", ""}, {"", "--output=f"}} {
		_, err := parseScope(false, c.base, c.commit, false)
		if err == nil || !strings.Contains(err.Error(), "must not start with -") {
			t.Errorf("parseScope(base=%q, commit=%q) err = %v, want the dash-ref error", c.base, c.commit, err)
		}
	}
}

// Diff given an option-shaped ref directly must treat it as a revision (--end-of-options), not a flag.
func TestDiffOptionShapedRefIsNotAFlag(t *testing.T) {
	dir := initRepo(t)
	out := filepath.Join(t.TempDir(), "pwned")
	for _, kind := range []string{"base", "commit"} {
		ref := "--output=" + out // base form becomes --output=<out>...HEAD: a flag without --end-of-options
		if _, err := diff(context.Background(), dir, diffScope{Kind: kind, Ref: ref}); err == nil {
			t.Errorf("%s: Diff with option-shaped ref succeeded, want a bad-revision error", kind)
		}
		if m, _ := filepath.Glob(out + "*"); len(m) > 0 {
			t.Fatalf("%s: git honored --output as a flag: %v", kind, m)
		}
	}
}

func TestUntrackedBlockMarkers(t *testing.T) {
	dir := initRepo(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("plain body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("ab\x00cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := untrackedBlock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"new file: plain.txt\nplain body", "new file: link.txt (symlink, skipped)", "new file: bin.dat (binary, skipped)"} {
		if !strings.Contains(got, want) {
			t.Errorf("block missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TOPSECRET") {
		t.Error("symlink target content was inlined")
	}
}

func TestUntrackedBlockTotalCap(t *testing.T) {
	dir := initRepo(t)
	chunk := bytes.Repeat([]byte("a"), untrackedCap)
	n := untrackedTotalCap/untrackedCap + 3
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+i))+".txt"), chunk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := untrackedBlock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "… 3 more untracked files not inlined") {
		t.Errorf("want the 3-file overflow line, got tail:\n%s", got[max(0, len(got)-120):])
	}
	if len(got) > untrackedTotalCap+2*untrackedCap {
		t.Errorf("block is %d bytes, exceeds the total cap", len(got))
	}
}

func TestDiffUntrackedTotalCapPartialFinalFile(t *testing.T) {
	dir := initRepo(t)
	sizes := []int{64, 64, 64, 64, 64, 64, 64, 63, 64}
	for i, kib := range sizes {
		name := "f" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("Z"), kib*1024), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := diff(context.Background(), dir, diffScope{Kind: "uncommitted"})
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(out, []byte("Z")); got != untrackedTotalCap {
		t.Fatalf("inlined %d content bytes, want exactly the %d-byte aggregate cap", got, untrackedTotalCap)
	}
	last := "new file: fi.txt\n" + strings.Repeat("Z", 1024) + "\n"
	if !strings.HasSuffix(string(out), last) {
		t.Fatal("final untracked file must inline only the remaining 1 KiB, excluding its header from the content cap")
	}
}

// commitGitFile rewrites a.txt to content and commits it.
func commitGitFile(t *testing.T, dir, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", msg)
}

// setOriginHEAD points refs/remotes/origin/HEAD at origin/<branch>, both at the current HEAD, with no network remote.
func setOriginHEAD(t *testing.T, dir, branch string) {
	t.Helper()
	runGit(t, dir, "update-ref", "refs/remotes/origin/"+branch, "HEAD")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

// review --pr diffs against origin/<default>, so commits sitting only on a stale-ahead local default are in scope.
func TestDiffPRUsesOriginDefaultBranch(t *testing.T) {
	dir := initRepo(t)
	setOriginHEAD(t, dir, "main")
	commitGitFile(t, dir, "one\nunpushed-on-local-main\n", "local only")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	commitGitFile(t, dir, "one\nunpushed-on-local-main\nfeature-line\n", "feature")

	out, err := diff(context.Background(), dir, diffScope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, want := range []string{"+feature-line", "+unpushed-on-local-main"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("pr diff missing %q: the base must be origin/main, not the local main:\n%s", want, out)
		}
	}
}

// A default branch named like a flag must reach git behind --end-of-options, not be parsed as an option.
func TestDiffPRPassesEndOfOptions(t *testing.T) {
	dir := initRepo(t)
	setOriginHEAD(t, dir, "-main")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	commitGitFile(t, dir, "one\nfeature-line\n", "feature")

	out, err := diff(context.Background(), dir, diffScope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff with default branch %q: %v", "-main", err)
	}
	if !strings.Contains(string(out), "feature-line") {
		t.Errorf("pr diff missing the feature commit:\n%s", out)
	}
}

func TestReviewArgsModelPrecedesPrompt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Given a model; When args are built; Then "--model <m>" sits right before the trailing prompt, and "" emits none
	args, cleanup, err := reviewArgs("gemini", diffScope{Kind: "uncommitted"}, []byte("d"), false, false, "", "", "gemini-4-argon-high")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	n := len(args)
	if n < 3 || args[n-3] != "--model" || args[n-2] != "gemini-4-argon-high" || args[n-1] != reviewPrompt(false, "") {
		t.Errorf("args tail = %v, want [--model gemini-4-argon-high <prompt>]", args[max(0, n-3):])
	}
	none, cleanup2, err := reviewArgs("gemini", diffScope{Kind: "uncommitted"}, []byte("d"), false, false, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if slices.Contains(none, "--model") {
		t.Errorf("args = %v, want no --model", none)
	}
}
