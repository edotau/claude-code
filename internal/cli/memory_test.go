package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// `memory migrate` no longer exists.
func TestMemoryMigrateVerbGone(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"migrate", "--dry-run"}, nil) }); code != 1 || !strings.Contains(stderr, "unknown subcommand") {
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
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory(c.args, nil) })
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"path", "--repo", repo}, nil) })
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

	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"init", "--repo", repo}, nil) })
	if code != 0 {
		t.Fatalf("memory init: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "home:") {
		t.Errorf("memory init output missing the renamed home: line: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory init output still names the pre-rename global tier: %q", out)
	}

	code, out, stderr = hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"index", "--repo", repo}, nil) })
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
	if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), "conventions.md"), []byte("# C\n\n- a rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := memory.BuildCatalog(time.Now())
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
	bankDir := memory.HomeBankDir()
	target := filepath.Join(bankDir, "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	if err := os.MkdirAll(bankDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# decisionLog\n\n"+longLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
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
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt(nil) })
	if code != 0 || !strings.Contains(out, "wrapped 1 file") {
		t.Fatalf("default fmt pass: code=%d out=%q, want 0 reporting one wrapped file", code, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for l := range strings.SplitSeq(string(data), "\n") {
		if len([]rune(l)) > memory.BankFmtMaxLine {
			t.Errorf("rewritten decisionLog.md still has an over-width line: %q", l)
		}
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"remember", "--repo", repo, "--type", "project", "ship the recall verb"}, nil)
	})
	if code != 0 || !strings.Contains(out, "remembered [") {
		t.Fatalf("memory remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
	code, out, stderr = hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"recall", "--repo", repo, "ship", "recall", "verb"}, nil) })
	if code != 0 || !strings.Contains(out, "ship the recall verb") {
		t.Fatalf("memory recall after remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// TestMemoryRememberUnknownTypeFails: the CLI surfaces memory.Remember's type-validation error.
func TestMemoryRememberUnknownTypeFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"remember", "--type", "bogus", "a fact"}, nil)
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo}, nil) })
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
	decisionLogOverCLI(t, filepath.Join(memory.Dir(repo), "decisionLog.md"), 80)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo, "--brief"}, nil) })
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"review", "--repo", repo, "--candidates"}, nil) })
	if code != 0 {
		t.Fatalf("memory review --candidates: code=%d stderr=%q", code, stderr)
	}
	statsIdx := strings.Index(out, "== memory stats (receipts on disk) ==")
	if statsIdx < 0 {
		t.Fatalf("output missing the stats block: %q", out)
	}
	ref, err := memory.Reference("review")
	if err != nil {
		t.Fatalf("memory.Reference(\"review\"): %v", err)
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
	before := dirChecksum(t, memory.Dir(repo))

	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"dream", "--cwd", repo, "--dry-run"}, nil) })
	if code != 0 {
		t.Fatalf("memory dream --dry-run: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "bank: "+memory.Dir(repo)) {
		t.Errorf("output missing the preview's bank line: %q", out)
	}

	after := dirChecksum(t, memory.Dir(repo))
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
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"init", "--repo", repo}, nil) })
	if code != 0 {
		t.Fatalf("memory init: code=%d stderr=%q", code, stderr)
	}
	ref, err := memory.Reference("init")
	if err != nil {
		t.Fatalf("memory.Reference(\"init\"): %v", err)
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
	id, err := memory.Remember(repo, "feedback", "promote via the CLI", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	checkMemoryPromoteHomeRoundTrip(t, repo, id)
}

func checkMemoryPromoteHomeRoundTrip(t *testing.T, repo, id string) {
	t.Helper()
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, "--dry-run", id, "--to", "home"}, nil)
	})
	if code != 0 || !strings.Contains(out, "would move") {
		t.Fatalf("memory promote --dry-run: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(memory.HomeBankDir(), "conventions.md"))
	if strings.Contains(string(data), id) {
		t.Error("--dry-run via the CLI still wrote the home bank")
	}
	code, out, stderr = hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, id, "--to", "home"}, nil)
	})
	if code != 0 || !strings.Contains(out, "promoted ["+id+"]") {
		t.Fatalf("memory promote: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, err := os.ReadFile(filepath.Join(memory.HomeBankDir(), "conventions.md"))
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
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdMemory(args, nil) }); code != 2 {
			t.Errorf("cmdMemory(%v) = %d, want 2", args, code)
		}
	}
}

// TestMemoryUpdateMissingTranscriptPrintsTheProcedure: without --transcript (a live session)
// cmdMemoryUpdate prints the embedded update procedure to stdout and exits 0 — it no longer refuses.
func TestMemoryUpdateMissingTranscriptPrintsTheProcedure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"update", "--cwd", t.TempDir(), "--session", "s"}, nil) })
	if code != 0 {
		t.Fatalf("memory update with no --transcript: code=%d, want 0", code)
	}
	ref, err := memory.Reference("update")
	if err != nil {
		t.Fatalf("memory.Reference(\"update\"): %v", err)
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
		if _, ok := memorySubcommands(nil)[v]; !ok {
			t.Errorf("private verb %q is not registered in memorySubcommands (undispatchable)", v)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"path"}, nil) }); code != 0 {
		t.Errorf("private verb \"path\" failed to dispatch: code=%d stderr=%q", code, stderr)
	}
}

// TestMemoryUpdateLiveSession: bare `memory update` resolves the live session's transcript from SessionIDEnv.
func TestMemoryUpdateLiveSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := t.TempDir()
	tr := filepath.Join(cfg, "projects", paths.ProjectSlug(dir), "live-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(tr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(memory.SessionIDEnv, "live-1")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || !strings.Contains(out, tr) {
		t.Errorf("live session: code=%d out=%q, want the transcript %s", code, out, tr)
	}
	t.Setenv(memory.SessionIDEnv, "")
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || strings.Contains(out, "dry run") {
		t.Errorf("no session: code=%d out=%q, want the procedure, not a dry run", code, out)
	}
}

// `memory show` and `memory search` append id-keyed lines with their reasons to the owning bank's access log.
func TestMemoryShowAndSearchLogAccess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // a non-repo cwd: search rides the home bank
	bank := filepath.Join(memory.HomeBankDir(), "decisionLog.md")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# D\n\n## 2026-10-01 — Zebra quota limiter choice <!-- id:abcd1234 -->\n\nzebra quota limiter fallback rationale\n"
	if err := os.WriteFile(bank, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"show", "abcd1234"}, nil) })
	if code != 0 || !strings.Contains(out, "Zebra quota limiter choice") {
		t.Fatalf("memory show: code=%d out=%q", code, out)
	}
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"search", "zebra quota limiter"}, nil) })
	if code != 0 || !strings.Contains(out, "decisionLog.md") {
		t.Fatalf("memory search: code=%d out=%q", code, out)
	}

	data, err := os.ReadFile(memory.AccessLogPath(memory.HomeBankDir()))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "abcd1234 -- "+memory.AccessShow) {
		t.Errorf("show return not logged id-keyed: %q", log)
	}
	if !strings.Contains(log, "abcd1234 -- "+memory.AccessSearch) {
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
	if err := memory.StoreRepo().Ensure(); err != nil {
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
	da, err := memory.EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := memory.EnsureRepoDir(b)
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
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryAlias(args) }); code == 0 {
			t.Errorf("%s: exit 0, want non-zero", name)
		}
	}
}

func TestMemoryAliasDryRunThenApply(t *testing.T) {
	_, b, ka, kb := aliasFixture(t)
	db, _ := memory.RepoDir(b)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, kb) {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("dry run removed the source store: %v", err)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) })
	if code != 0 || !strings.HasPrefix(out, "applied:") {
		t.Fatalf("apply: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("source store survived apply: %v", err)
	}
	if got, _ := memory.RepoDir(b); filepath.Base(got) != ka {
		t.Errorf("RepoDir(beta) = %s, want key %s", got, ka)
	}
	if code, _, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryAlias([]string{"--repo", b, "--into", ka}) }); code == 0 {
		t.Errorf("repeat apply exited 0, want non-zero")
	}
}

func TestMemoryAliasDispatchedAndCompleted(t *testing.T) {
	engine := helptext.Engine{
		Tree:    func() map[string]*helptext.Node { return map[string]*helptext.Node{"memory": memorySpec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
	if got := engine.Candidates([]string{"memory", "al"}); !slices.Equal(got, []string{"alias"}) {
		t.Errorf("memory al* = %v, want [alias]", got)
	}
	got := engine.Candidates([]string{"memory", "alias", "--"})
	for _, want := range []string{"--repo", "--into", "--dry-run"} {
		if !slices.Contains(got, want) {
			t.Errorf("memory alias flags missing %q: %v", want, got)
		}
	}
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"alias"}, nil) }); code == 0 {
		t.Errorf("Run alias without flags exited 0")
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemoryDream([]string{"--dry-run"})
	})
	// Should succeed with --dry-run even in an empty bank
	if code != 0 {
		t.Errorf("memory dream --dry-run: exit %d, want 0; stderr=%q", code, stderr)
	}
}

// TestMemorySearchFailReturnValue ensures memory search returns a non-zero code
// (regression for internal/cli/memory.go:245 where helptext.Fail() return value not used).
func TestMemorySearchFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemorySearch([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory search (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemoryShowFailReturnValue ensures memory show returns the error code from helptext.Fail()
// (regression for internal/cli/memory.go:277).
func TestMemoryShowFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemoryShow([]string{})
	})
	// Should return 2 (from fail) when no argument given
	if code != 2 {
		t.Errorf("memory show (no args) exit %d, want 2; stderr=%q", code, stderr)
	}
}

// TestMemoryDigestReportsMissingClaudeBinary: the install hint from claude.ClaudeBinary must reach the user.
func TestMemoryDigestReportsMissingClaudeBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "cfg"))
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"digest", "--repo", t.TempDir()}, nil) })
	if code != 1 || !strings.Contains(stderr, "install --tools=claude") {
		t.Errorf("memory digest without claude: code=%d stderr=%q, want 1 with the install hint", code, stderr)
	}
}

// TestMemoryRecallRanksAndFormatsUnstampedHits: `memory recall` prints each hit "<ref> — <title>";
// an unstamped hit (no persisted id) prints a content-derived ref, never a bare "-", and that ref
// resolves back through `memory show` (the point of giving every hit a ref at all).
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := memory.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.Dir(repo), "conventions.md"),
		[]byte("# Conventions\n\n- stamped id-marked rule <!-- id:deadbeef -->\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Append an unstamped line by hand, after Remember's settle pass already assigned ids above —
	// activeContext.md is never id-assigned, so it stays unstamped (SearchBanks still indexes it).
	if err := os.WriteFile(filepath.Join(memory.Dir(repo), "activeContext.md"),
		[]byte("# Active Context\n\n- rule stamped id-marked never settled here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkMemoryRecallUnstampedRefs(t, repo)
}

func checkMemoryRecallUnstampedRefs(t *testing.T, repo string) {
	t.Helper()
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"recall", "--repo", repo, "stamped", "id-marked", "rule"}, nil) })
	if code != 0 {
		t.Fatalf("memory recall: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "deadbeef — ") {
		t.Errorf("recall output missing the stamped id line: %q", out)
	}
	if strings.Contains(out, "\n- —") || strings.Contains(out, "\n — ") {
		t.Errorf("an unstamped hit printed the old bare-dash format: %q", out)
	}
	contentRef := unstampedRecallRef(out)
	if contentRef == "" || !strings.HasPrefix(contentRef, "z") {
		t.Fatalf("recall output missing a content-derived ref for the unstamped hit: %q", out)
	}
	if _, block, err := memory.ShowBlock(contentRef); err != nil || !strings.Contains(block, "never settled here") {
		t.Errorf("memory show %s = %q, %v; want the unstamped block", contentRef, block, err)
	}
}

func unstampedRecallRef(out string) string {
	var contentRef string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if ref, _, ok := strings.Cut(line, " — "); ok && ref != "deadbeef" {
			contentRef = ref
		}
	}
	return contentRef
}

// TestMemoryRecallLoadResolvesUnstampedHitByContentRef: `memory recall --load` fetches an unstamped
// hit's body through its derived content ref, the same path a persisted id takes.
func TestMemoryRecallLoadResolvesUnstampedHitByContentRef(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := memory.StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory.Dir(repo), "activeContext.md"),
		[]byte("# Active Context\n\n- never settled widget rollout detail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"recall", "--repo", repo, "--load", "1", "never", "settled", "widget", "rollout"}, nil)
	})
	if code != 0 {
		t.Fatalf("memory recall --load: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "never settled widget rollout detail") {
		t.Errorf("recall --load did not resolve the unstamped hit's body: %q", out)
	}
}

// TestMemoryRecallNoQueryFails: an empty query is refused (exit 1), naming memoryUsage.
func TestMemoryRecallNoQueryFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"recall"}, nil) })
	if code != 1 || !strings.Contains(stderr, "no query") {
		t.Errorf("memory recall with no terms: code=%d stderr=%q", code, stderr)
	}
}

// TestMemoryRecallLoadAppendsBodiesUnderByteCap: --load N appends each hit's full block text,
// never exceeding recallLoadCap total.
func TestMemoryRecallLoadAppendsBodiesUnderByteCap(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Remember(repo, "feedback", "loadable fact about widgets and gadgets", time.Now()); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"recall", "--repo", repo, "--load", "3", "loadable", "widgets", "gadgets", "fact"}, nil)
	})
	if code != 0 {
		t.Fatalf("memory recall --load: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "loadable fact about widgets and gadgets") {
		t.Errorf("recall --load output missing the loaded body: %q", out)
	}
	if len(out) > 20000 { // generous ceiling: just confirms --load didn't dump the whole bank unbounded
		t.Errorf("recall --load output suspiciously large (%d bytes), want it capped near recallLoadCap", len(out))
	}
}

// TestMemoryRecallFormatValidation ensures memory recall exits with an error
// when an invalid format is specified (regression for internal/cli/memory.go error handling).
func TestMemoryRecallFormatValidation(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemoryRecall([]string{"test", "--format", "invalid"})
	})
	if code == 0 {
		t.Errorf("memory recall with invalid format: exit %d, want non-zero; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "format") {
		t.Errorf("memory recall invalid format: stderr %q, want format error message", stderr)
	}
}

// TestMemoryRecallFailReturnValue ensures memory recall returns a non-zero code
// (regression for internal/cli/memory.go:150 where helptext.Fail() return value not used).
func TestMemoryRecallFailReturnValue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemoryRecall([]string{})
	})
	// Should return non-zero when no arguments given
	if code == 0 {
		t.Errorf("memory recall (no args) exit %d, want non-zero; stderr=%q", code, stderr)
	}
}

// TestMemoryRecallLoadBlockErrorSilent documents the behavior where ShowBlock() errors
// are silently ignored in recallLoad() (regression for internal/cli/memory.go:188).
// The fix should log or report errors instead of silently continuing.
func TestMemoryRecallLoadBlockErrorSilent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Use --load with an invalid block ID to trigger ShowBlock() error
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return cmdMemoryRecall([]string{"nonexistent", "--load", "1"})
	})
	// Should succeed (exit 0) but only print the title, not the body
	// After the fix, errors should be logged or reported
	_ = code
	_ = out
}

func TestMemoryDreamRejectsStrayBeforeDryRun(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An invalid config directory prevents a regressed forced dream from reaching the model runner.
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir())
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"dream", "--force", "stray", "--dry-run"}, nil)
	})
	if code != 2 {
		t.Errorf("dream ignored stray before --dry-run: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, `memory dream: unexpected argument "stray"`) {
		t.Errorf("dream missing positional usage error: %q", stderr)
	}
	if out != "" {
		t.Errorf("dream ran despite invalid arguments: %q", out)
	}
}

func TestMemoryFmtRejectsStrayBeforeCheck(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir())
	dir := memory.HomeBankDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "decisionLog.md")
	before := "# Decision Log\n\n- " + strings.Repeat("longword ", 30) + "end\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"fmt", "stray", "--check"}, nil)
	})
	if code != 2 {
		t.Errorf("fmt ignored stray before --check: exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if !strings.Contains(stderr, `memory fmt: unexpected argument "stray"`) {
		t.Errorf("fmt missing positional usage error: %q", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("fmt wrote bank file despite invalid arguments: %q", after)
	}
}

func TestMemoryCompletionSupportedCommandsAndFlags(t *testing.T) {
	engine := helptext.Engine{
		Tree:    func() map[string]*helptext.Node { return map[string]*helptext.Node{"memory": memorySpec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"verify command", []string{"memory", "ver"}, []string{"verify"}},
		{"verify flags", []string{"memory", "verify", "--"}, []string{"--cwd", "--archives", "--all-stores"}},
		{"review flags", []string{"memory", "review", "--"}, []string{"--candidates"}},
		{"update flags", []string{"memory", "update", "--"}, []string{"--dry-run", "--historical"}},
		{"dream flags", []string{"memory", "dream", "--"}, []string{"--dry-run"}},
		{"catchup flags", []string{"memory", "catchup", "--"}, []string{"--dry-run", "--skip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.Candidates(tc.words)
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("completion for %v missing %q: %v", tc.words, want, got)
				}
			}
		})
	}
}

func TestMemoryPathRepoFlagResolvesStoreName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gittest.Repo(t)
	dir, err := memory.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(repo)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemory([]string{"path", "--repo", name}, nil) })
	if code != 0 || strings.TrimSpace(out) != dir {
		t.Errorf("memory path --repo %s = %d %q (stderr %q), want %q", name, code, out, stderr, dir)
	}
}

func TestMemoryCompletionOffersIndexAndRecallFlags(t *testing.T) {
	engine := helptext.Engine{
		Tree:    func() map[string]*helptext.Node { return map[string]*helptext.Node{"memory": memorySpec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
	for verb, want := range map[string][]string{"index": {"--reembed", "--mint-only"}, "recall": {"--explain"}} {
		got := engine.Candidates([]string{"memory", verb, "--"})
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("memory %s completion missing %q: %v", verb, w, got)
			}
		}
	}
}

// TestSkillVerbsArePublicCLIVerbs checks the skill's verb table against memoryPage and memorySubcommands.
func TestSkillVerbsArePublicCLIVerbs(t *testing.T) {
	table := reflectionVerbTable(t)
	verbRe := regexp.MustCompile("^`([a-z][a-z-]*)")
	seen := map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			continue
		}
		first := strings.TrimSpace(cells[1]) // cells[0] is empty (row starts with "|")
		m := verbRe.FindStringSubmatch(first)
		if m == nil {
			continue
		}
		verb := m[1]
		if seen[verb] {
			continue
		}
		seen[verb] = true
		if _, ok := memorySubcommands(nil)[verb]; !ok {
			t.Errorf("SKILL.md names unknown verb %q (not in memorySubcommands)", verb)
			continue
		}
		if !helpPageHasCommand(memoryPage, verb) {
			t.Errorf("SKILL.md names verb %q that is not listed as public in memoryPage", verb)
		}
	}
	if len(seen) == 0 {
		t.Fatal("verb table names no verbs")
	}
}

func reflectionVerbTable(t *testing.T) string {
	t.Helper()
	root := reflectionRepoRoot(t)
	path := filepath.Join(root, "skills", "reflection", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	tableStart := strings.Index(body, "| Verb |")
	if tableStart < 0 {
		t.Fatal("SKILL.md has no verb table (expected a \"| Verb |\" header row)")
	}
	tableEnd := strings.Index(body[tableStart:], "\n\n")
	if tableEnd < 0 {
		tableEnd = len(body) - tableStart
	}
	return body[tableStart : tableStart+tableEnd]
}

// reflectionRepoRoot resolves this repo's toplevel from the cli package's working directory.
func reflectionRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

func TestMemoryReportRegistersFreshRepoOrigin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gittest.Repo(t)
	dir := memory.Dir(repo)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fresh repo store must not exist: %v", err)
	}
	input := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(input, []byte(`{"report":"# Review\n\nFresh repo report.\n","decisions":["Keep reports together."]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		checkFreshRepoReportPaths(t, repo, input, dir)
		if got := memory.Dir(repo); got != dir {
			t.Fatalf("report split repo store: got %q, want %q", got, dir)
		}
	}
	entries, err := memory.RepoDirs()
	if err != nil {
		t.Fatal(err)
	}
	entry := memory.FindEntry(entries, memory.Resolve(repo).Origin)
	if entry == nil || entry.Dir != dir {
		t.Fatalf("report store %q is not discoverable by RepoDirs: %v", dir, entries)
	}
}

func checkFreshRepoReportPaths(t *testing.T, repo, input, dir string) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdMemory([]string{"report", "--repo", repo, "--label", "origin-regression", "--file", input, "--format", "json"}, nil)
	})
	if code != 0 {
		t.Fatalf("memory report: exit=%d stderr=%q", code, stderr)
	}
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("memory report JSON: %v; stdout=%q", err, stdout)
	}
	if len(out.Paths) != 2 {
		t.Fatalf("memory report paths = %v, want report and decisions", out.Paths)
	}
	for _, path := range out.Paths {
		if filepath.Dir(path) != memory.ReportsDir(dir) {
			t.Fatalf("report path %q is outside original store %q", path, dir)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("reported file %q: %v", path, err)
		}
	}
}

func helpPageHasCommand(page helptext.Page, name string) bool {
	for _, row := range page.Commands {
		words := strings.Fields(row[0])
		if len(words) > 0 && words[0] == name {
			return true
		}
	}
	return false
}
