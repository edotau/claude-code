package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
)

func TestMemoryRootHonorsHarnessWorktreePointer(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	worktree := t.TempDir()
	pointer := "gitdir: " + filepath.Join(cfg, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", worktree)
	if got := memstore.Root(""); got != worktree {
		t.Fatalf("memstore.Root(\"\") = %q, want harness worktree %q (not cwd %q)", got, worktree, other)
	}
}

// The inverse: CLAUDE_CONFIG_DIR is itself a linked worktree, so its main checkout is not the config dir.
func TestMemoryRootHonorsConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pointer := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(cfg, ".git"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", cfg)
	if got := memstore.Root(""); got != cfg {
		t.Fatalf("memstore.Root(\"\") = %q, want config worktree %q (not cwd %q)", got, cfg, other)
	}
}

// A CLAUDE_PROJECT_DIR that is not a repo (a launcher stamping $HOME) must not win over the cwd's toplevel.
func TestMemoryRootIgnoresNonRepoProjectDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	t.Chdir(filepath.Join(repo, "sub"))
	want, _ := filepath.EvalSymlinks(repo)

	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Dir(repo)) // the non-repo parent
	if got, _ := filepath.EvalSymlinks(memstore.Root("")); got != want {
		t.Errorf("non-repo CLAUDE_PROJECT_DIR won: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo) // a real repo root is honored
	if got, _ := filepath.EvalSymlinks(memstore.Root("")); got != want {
		t.Errorf("repo CLAUDE_PROJECT_DIR not honored: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", cfg) // the config dir itself is the global bank's root
	if got := memstore.Root(""); got != cfg {
		t.Errorf("config dir not honored: got %s", got)
	}
}

// `memory migrate` no longer exists.
func TestMemoryMigrateVerbGone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"migrate", "--dry-run"}) }); code != 1 || !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("memory migrate: code=%d stderr=%q, want exit 1 unknown subcommand", code, stderr)
	}
}

// TestMemorySubcommandsWithoutRepoRejectBareRepoFlag: fmt/update/dream/show take no --repo flag —
// their own flag.FlagSet has nothing registered for it, so passing it is a parse error (exit 2).
func TestMemorySubcommandsWithoutRepoRejectBareRepoFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"fmt", "--repo", "/x"}, 2},
		{[]string{"update", "--repo", "/x"}, 2},
		{[]string{"dream", "--repo", "/x"}, 2},
		{[]string{"show", "--repo", "/x"}, 2},
	}
	for _, c := range cases {
		code, _, stderr := captureOutput(t, func() int { return cmdMemory(c.args) })
		if code != c.want {
			t.Errorf("cmdMemory(%v) = %d, want %d; stderr=%q", c.args, code, c.want, stderr)
		}
	}
}

// TestMemoryPathHonorsRepoFlag: `memory path --repo <dir>` does take --repo, and exits 0 with the
// bank dir under that repo.
func TestMemoryPathHonorsRepoFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"path", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory path --repo %s: code = %d, want 0; stderr=%q", repo, code, stderr)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("memory path --repo: empty output, want a bank dir")
	}
}

// TestNoPreRenameLabelInUserOutput: user-visible output from `memory init`, `memory index` and
// the rendered catalog names the renamed "home" tier, never the pre-rename "global" label.
func TestNoPreRenameLabelInUserOutput(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"init", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory init: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "home:") {
		t.Errorf("memory init output missing the renamed home: line: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory init output still names the pre-rename global tier: %q", out)
	}

	code, out, stderr = captureOutput(t, func() int { return cmdMemory([]string{"index", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory index: code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory index output still names the pre-rename global tier: %q", out)
	}

	checkMemoryCatalogHomeLabel(t)
}

func checkMemoryCatalogHomeLabel(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(memstore.HomeBankDir(), "conventions.md"), []byte("# C\n\n- a rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := bank.BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(catalog, "## Home (memory/home/)") {
		t.Errorf("catalog heading not renamed to Home: %q", catalog)
	}
	if strings.Contains(strings.ToLower(catalog), "global") {
		t.Errorf("rendered catalog still names the pre-rename global tier: %q", catalog)
	}
}

// TestCmdMemoryFmt: --check is clean (exit 0) on a well-formed bank, reports the over-width file and
// exits 1 without writing it, and the default pass wraps it and exits 0; the usage string and the
// settle-writer guard both name "fmt".
func TestCmdMemoryFmt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bankDir := memstore.HomeBankDir()
	target := filepath.Join(bankDir, "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	if err := os.MkdirAll(bankDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# decisionLog\n\n"+longLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := captureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 1 || !strings.Contains(out, target) {
		t.Fatalf("--check over a dirty bank: code=%d out=%q, want 1 naming %s", code, out, target)
	}
	if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), longLine) {
		t.Fatalf("--check wrote the file: %q err=%v", data, err)
	}

	checkMemoryFmtWrapAndClean(t, target)

	if !strings.Contains(memoryPage.Hint, "fmt") {
		t.Error("memoryPage's plumbing hint does not list `fmt`")
	}
}

func checkMemoryFmtWrapAndClean(t *testing.T, target string) {
	t.Helper()
	code, out, _ := captureOutput(t, func() int { return cmdMemoryFmt(nil) })
	if code != 0 || !strings.Contains(out, "wrapped 1 file") {
		t.Fatalf("default fmt pass: code=%d out=%q, want 0 reporting one wrapped file", code, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(string(data), "\n") {
		if len([]rune(l)) > bank.BankFmtMaxLine {
			t.Errorf("rewritten decisionLog.md still has an over-width line: %q", l)
		}
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 0 || !strings.Contains(out, "clean") {
		t.Errorf("--check after wrapping: code=%d out=%q, want 0 clean", code, out)
	}
}

// TestMemoryRememberCLIRoundTripsThroughRecall: `memory remember` writes a typed entry findable by
// `memory recall` on its own words.
func TestMemoryRememberCLIRoundTripsThroughRecall(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"remember", "--repo", repo, "--type", "project", "ship the recall verb"})
	})
	if code != 0 || !strings.Contains(out, "remembered [") {
		t.Fatalf("memory remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
	code, out, stderr = captureOutput(t, func() int { return cmdMemory([]string{"recall", "--repo", repo, "ship", "recall", "verb"}) })
	if code != 0 || !strings.Contains(out, "ship the recall verb") {
		t.Fatalf("memory recall after remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// TestMemoryRememberUnknownTypeFails: the CLI surfaces bank.Remember's type-validation error.
func TestMemoryRememberUnknownTypeFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"remember", "--type", "bogus", "a fact"})
	})
	if code != 1 || !strings.Contains(stderr, "want") {
		t.Errorf("memory remember --type bogus: code=%d stderr=%q", code, stderr)
	}
}

// TestMemoryReviewContainsDoctorAndStatsBlocks: `memory review` folds the old doctor+stats output —
// both the problem-report line and the "memory stats" receipts block must be present.
func TestMemoryReviewContainsDoctorAndStatsBlocks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory review: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "memory doctor: no problems found") {
		t.Errorf("review output missing the doctor block: %q", out)
	}
	if !strings.Contains(out, "== memory stats (receipts on disk) ==") {
		t.Errorf("review output missing the stats block: %q", out)
	}
}

// TestMemoryReviewBriefPrintsOnlyCount: --brief collapses the per-line problem report to a count,
// the stats block still prints.
func TestMemoryReviewBriefPrintsOnlyCount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	decisionLogOverCLI(t, filepath.Join(memstore.Dir(repo), "decisionLog.md"), 80)
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo, "--brief"}) })
	if code != 0 {
		t.Fatalf("memory review --brief: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "problem(s) found") {
		t.Errorf("--brief output missing the count line: %q", out)
	}
	if strings.Contains(out, "- decisionLog.md") {
		t.Errorf("--brief printed a per-line problem, want only the count: %q", out)
	}
}

// TestMemoryReviewCandidatesPrintsProcedureAfterStats: --candidates appends the embedded review
// procedure after the doctor/stats blocks — the ranking pass a human/agent applies by hand.
func TestMemoryReviewCandidatesPrintsProcedureAfterStats(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo, "--candidates"}) })
	if code != 0 {
		t.Fatalf("memory review --candidates: code=%d stderr=%q", code, stderr)
	}
	statsIdx := strings.Index(out, "== memory stats (receipts on disk) ==")
	if statsIdx < 0 {
		t.Fatalf("output missing the stats block: %q", out)
	}
	ref, err := bank.Reference("review")
	if err != nil {
		t.Fatalf("bank.Reference(\"review\"): %v", err)
	}
	refIdx := strings.Index(out, ref)
	if refIdx < 0 {
		t.Errorf("output missing the embedded review procedure: %q", out)
	}
	if refIdx < statsIdx {
		t.Errorf("review procedure (at %d) printed before the stats block (at %d)", refIdx, statsIdx)
	}
}

// TestMemoryDreamDryRunWritesNothing: --dry-run prints PreviewDream's deterministic report and
// never touches the bank — checksummed before/after, no DreamDue gate consulted, no agent run.
func TestMemoryDreamDryRunWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	before := dirChecksum(t, memstore.Dir(repo))

	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"dream", "--cwd", repo, "--dry-run"}) })
	if code != 0 {
		t.Fatalf("memory dream --dry-run: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "bank: "+memstore.Dir(repo)) {
		t.Errorf("output missing the preview's bank line: %q", out)
	}

	after := dirChecksum(t, memstore.Dir(repo))
	if before != after {
		t.Errorf("--dry-run changed the bank directory: before=%s after=%s", before, after)
	}
}

// dirChecksum sums every regular file's size+modtime under dir — enough to detect any write
// without reading file bytes; used to prove a --dry-run path touched nothing.
func dirChecksum(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(&b, "%s|%d|%s\n", path, fi.Size(), fi.ModTime())
		return nil
	})
	return b.String()
}

// TestMemoryInitPrintsProcedureOnFreshBank: a fresh `memory init` leaves projectContext.md as the
// template, so init prints the embedded init procedure after the scaffold summary.
func TestMemoryInitPrintsProcedureOnFreshBank(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"init", "--repo", repo}) })
	if code != 0 {
		t.Fatalf("memory init: code=%d stderr=%q", code, stderr)
	}
	ref, err := bank.Reference("init")
	if err != nil {
		t.Fatalf("bank.Reference(\"init\"): %v", err)
	}
	if !strings.Contains(out, ref) {
		t.Errorf("output missing the embedded init procedure: %q", out)
	}
}

// decisionLogOverCLI writes n decisionLog.md entries so `memory review` has a cap problem to report.
func decisionLogOverCLI(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Decision Log\n")
	for i := n; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## decision %02d\n\nrationale topic%d choice%d\n", i, i, i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryPromoteCLIHomeMovesBlockAndDryRunWritesNothing: the CLI wiring for `memory promote`
// round-trips through memory.Promote for both the real move and --dry-run.
func TestMemoryPromoteCLIHomeMovesBlockAndDryRunWritesNothing(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	id, err := bank.Remember(repo, "feedback", "promote via the CLI", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	checkMemoryPromoteHomeRoundTrip(t, repo, id)
}

func checkMemoryPromoteHomeRoundTrip(t *testing.T, repo, id string) {
	t.Helper()
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, "--dry-run", id, "--to", "home"})
	})
	if code != 0 || !strings.Contains(out, "would move") {
		t.Fatalf("memory promote --dry-run: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(memstore.HomeBankDir(), "conventions.md"))
	if strings.Contains(string(data), id) {
		t.Error("--dry-run via the CLI still wrote the home bank")
	}
	code, out, stderr = captureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, id, "--to", "home"})
	})
	if code != 0 || !strings.Contains(out, "promoted ["+id+"]") {
		t.Fatalf("memory promote: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, err := os.ReadFile(filepath.Join(memstore.HomeBankDir(), "conventions.md"))
	if err != nil || !strings.Contains(string(data), id) {
		t.Fatalf("home bank missing the promoted id after the real run: %v %q", err, data)
	}
}

// TestMemoryPromoteCLIMissingArgsFail: a missing --to, or extra positional args, is a usage error (exit 2).
func TestMemoryPromoteCLIMissingArgsFail(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := [][]string{
		{"promote", "abc123"},                          // no --to
		{"promote", "abc123", "extra", "--to", "home"}, // two positionals
	}
	for _, args := range cases {
		if code, _, _ := captureOutput(t, func() int { return cmdMemory(args) }); code != 2 {
			t.Errorf("cmdMemory(%v) = %d, want 2", args, code)
		}
	}
}

// TestMemoryUpdateMissingTranscriptPrintsTheProcedure: without --transcript (a live session)
// cmdMemoryUpdate prints the embedded update procedure to stdout and exits 0 — it no longer refuses.
func TestMemoryUpdateMissingTranscriptPrintsTheProcedure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, _ := captureOutput(t, func() int { return cmdMemory([]string{"update", "--cwd", t.TempDir(), "--session", "s"}) })
	if code != 0 {
		t.Fatalf("memory update with no --transcript: code=%d, want 0", code)
	}
	ref, err := bank.Reference("update")
	if err != nil {
		t.Fatalf("bank.Reference(\"update\"): %v", err)
	}
	if !strings.Contains(out, ref) {
		t.Errorf("stdout missing the embedded update procedure: %q", out)
	}
}

// TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch checks public rows and private dispatch.
func TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch(t *testing.T) {
	publicVerbs := []string{"recall", "remember", "init", "review", "promote", "update", "dream"}
	for _, v := range publicVerbs {
		if !helpPageHasCommand(memoryPage, v) {
			t.Errorf("memoryPage missing public verb %q", v)
		}
	}
	privateVerbs := []string{"search", "show", "index", "path", "digest", "fmt", "watch", "timeline", "archive", "alias"}
	for _, v := range privateVerbs {
		if helpPageHasCommand(memoryPage, v) {
			t.Errorf("memoryPage lists private verb %q as if public", v)
		}
		if _, ok := memorySubcommands[v]; !ok {
			t.Errorf("private verb %q is not registered in memorySubcommands (undispatchable)", v)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"path"}) }); code != 0 {
		t.Errorf("private verb \"path\" failed to dispatch: code=%d stderr=%q", code, stderr)
	}
}

// TestMemoryUpdateLiveSession: bare `memory update` resolves the live session's transcript from SessionIDEnv.
func TestMemoryUpdateLiveSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	tr := filepath.Join(cfg, "projects", memstore.ProjectSlug(dir), "live-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(tr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(memory.SessionIDEnv, "live-1")
	code, out, _ := captureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || !strings.Contains(out, tr) {
		t.Errorf("live session: code=%d out=%q, want the transcript %s", code, out, tr)
	}
	t.Setenv(memory.SessionIDEnv, "")
	code, out, _ = captureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || strings.Contains(out, "dry run") {
		t.Errorf("no session: code=%d out=%q, want the procedure, not a dry run", code, out)
	}
}

// `memory show` and `memory search` append id-keyed lines with their reasons to the owning bank's access log.
func TestMemoryShowAndSearchLogAccess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // a non-repo cwd: search rides the home bank
	bank := filepath.Join(memstore.HomeBankDir(), "decisionLog.md")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# D\n\n## 2026-10-01 — Zebra quota limiter choice <!-- id:abcd1234 -->\n\nzebra quota limiter fallback rationale\n"
	if err := os.WriteFile(bank, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := captureOutput(t, func() int { return cmdMemory([]string{"show", "abcd1234"}) })
	if code != 0 || !strings.Contains(out, "Zebra quota limiter choice") {
		t.Fatalf("memory show: code=%d out=%q", code, out)
	}
	code, out, _ = captureOutput(t, func() int { return cmdMemory([]string{"search", "zebra quota limiter"}) })
	if code != 0 || !strings.Contains(out, "decisionLog.md") {
		t.Fatalf("memory search: code=%d out=%q", code, out)
	}

	data, err := os.ReadFile(recall.AccessLogPath(memstore.HomeBankDir()))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "abcd1234 -- "+recall.AccessShow) {
		t.Errorf("show return not logged id-keyed: %q", log)
	}
	if !strings.Contains(log, "abcd1234 -- "+recall.AccessSearch) {
		t.Errorf("search return not logged id-keyed: %q", log)
	}
}

func aliasGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// aliasFixture: isolated config with a cut-over store and two checkouts, each with its own store.
func aliasFixture(t *testing.T) (a, b, ka, kb string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Join(cfg, "state", "harness", "migrate"), 0o755); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(cfg, "state", "harness", "migrate", "20260101-000000.journal.json")
	if err := os.WriteFile(j, []byte(`{"phase":"complete","manifest":"/m/done.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bank.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	for _, n := range []string{"alpha", "beta"} {
		r := filepath.Join(parent, n)
		os.MkdirAll(r, 0o755)
		aliasGit(t, r, "init", "-q", "-b", "main")
		aliasGit(t, r, "commit", "-q", "--allow-empty", "-m", "init")
	}
	a, b = filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	da, err := memstore.EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := memstore.EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(db, "topic.md"), []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return a, b, filepath.Base(da), filepath.Base(db)
}

func TestMemoryAliasUsageErrors(t *testing.T) {
	a, _, ka, _ := aliasFixture(t)
	for name, args := range map[string][]string{
		"no flags":    {},
		"no into":     {"--repo", a},
		"no repo":     {"--into", ka},
		"extra arg":   {"--repo", a, "--into", ka, "stray"},
		"unknown key": {"--repo", a, "--into", "nope"},
		"self":        {"--repo", a, "--into", ka},
		"bad flag":    {"--bogus"},
	} {
		if code, _, _ := captureOutput(t, func() int { return cmdMemoryAlias(args) }); code == 0 {
			t.Errorf("%s: exit 0, want non-zero", name)
		}
	}
}

func TestMemoryAliasDryRunThenApply(t *testing.T) {
	_, b, ka, kb := aliasFixture(t)
	db, _ := memstore.RepoDir(b)

	code, out, _ := captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, kb) {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("dry run removed the source store: %v", err)
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) })
	if code != 0 || !strings.HasPrefix(out, "applied:") {
		t.Fatalf("apply: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("source store survived apply: %v", err)
	}
	if got, _ := memstore.RepoDir(b); filepath.Base(got) != ka {
		t.Errorf("RepoDir(beta) = %s, want key %s", got, ka)
	}
	if code, _, _ = captureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) }); code == 0 {
		t.Errorf("repeat apply exited 0, want non-zero")
	}
}

func TestMemoryAliasDispatchedAndCompleted(t *testing.T) {
	if got := completionCandidates([]string{"memory", "al"}); !slices.Equal(got, []string{"alias"}) {
		t.Errorf("memory al* = %v, want [alias]", got)
	}
	got := completionCandidates([]string{"memory", "alias", "--"})
	for _, want := range []string{"--repo", "--into", "--dry-run"} {
		if !slices.Contains(got, want) {
			t.Errorf("memory alias flags missing %q: %v", want, got)
		}
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemory([]string{"alias"}) }); code == 0 {
		t.Errorf("cmdMemory alias without flags exited 0")
	}
}

// TestMemorySearchJSONWriteError ensures memory search --format json exits non-zero
// when writing to stdout fails (regression test for internal/cli/memory.go:158).
func TestMemorySearchJSONWriteError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()

	code := cmdMemorySearch([]string{"anyterm", "--format", "json"})
	if code == 0 {
		t.Errorf("memory search --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestMemorySearchFormatValidation ensures memory search exits with an error
// when an invalid format is specified (regression for internal/cli/memory.go error handling).
func TestMemorySearchFormatValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemorySearch([]string{"test", "--format", "invalid"})
	})
	if code == 0 {
		t.Errorf("memory search with invalid format: exit %d, want non-zero; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "format") {
		t.Errorf("memory search invalid format: stderr %q, want format error message", stderr)
	}
}

// TestMemoryDreamGetWdError ensures memory dream handles os.Getwd() errors gracefully
// (regression for internal/cli/memory.go:358).
func TestMemoryDreamGetWdError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Test memory dream without --cwd to exercise the Getwd() path
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryDream([]string{"--dry-run"})
	})
	// Should succeed with --dry-run even in an empty bank
	if code != 0 {
		t.Errorf("memory dream --dry-run: exit %d, want 0; stderr=%q", code, stderr)
	}
}

// TestMemorySearchFailReturnValue ensures memory search returns a non-zero code
// (regression for internal/cli/memory.go:245 where fail() return value not used).
func TestMemorySearchFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemorySearch([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory search (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemoryShowFailReturnValue ensures memory show returns the error code from fail()
// (regression for internal/cli/memory.go:277).
func TestMemoryShowFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemoryShow([]string{})
	})
	// Should return 2 (from fail) when no argument given
	if code != 2 {
		t.Errorf("memory show (no args) exit %d, want 2; stderr=%q", code, stderr)
	}
}
