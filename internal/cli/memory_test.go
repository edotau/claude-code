package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
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
	if got := memoryRoot(""); got != worktree {
		t.Fatalf("memoryRoot(\"\") = %q, want harness worktree %q (not cwd %q)", got, worktree, other)
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
	if got := memoryRoot(""); got != cfg {
		t.Fatalf("memoryRoot(\"\") = %q, want config worktree %q (not cwd %q)", got, cfg, other)
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
	if got, _ := filepath.EvalSymlinks(memoryRoot("")); got != want {
		t.Errorf("non-repo CLAUDE_PROJECT_DIR won: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo) // a real repo root is honored
	if got, _ := filepath.EvalSymlinks(memoryRoot("")); got != want {
		t.Errorf("repo CLAUDE_PROJECT_DIR not honored: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", cfg) // the config dir itself is the global bank's root
	if got := memoryRoot(""); got != cfg {
		t.Errorf("config dir not honored: got %s", got)
	}
}

// memory migrate: the default dry run writes only a manifest (text and json), --origin rejects a bare value,
// a foreign manifest is refused with exit 2, and `memory update` is refused while maintenance is raised.
func TestMemoryMigrateDryRunApplyAndGuard(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	bank := filepath.Join(cfg, "docs", "memory", "bank")
	if err := os.MkdirAll(bank, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bank, "conventions.md"), []byte("# C\n- legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--dry-run"}) })
	if code != 0 || !strings.Contains(out, "global-bank") || !strings.Contains(out, "manifest: ") || !strings.Contains(out, "memory migrate --manifest ") {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "- legacy") {
		t.Error("dry run printed file contents")
	}
	manifests, _ := filepath.Glob(filepath.Join(memory.MigrateStateDir(), "manifest-*.json"))
	if len(manifests) != 1 {
		t.Fatalf("manifests after dry run: %v", manifests)
	}
	if _, err := os.Stat(memory.StoreDir()); !os.IsNotExist(err) {
		t.Error("dry run created the store")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // WriteManifest names to the second: a fresh dir avoids the O_EXCL clash
	code, out, _ = captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--dry-run", "--format", "json"}) })
	var m memory.Manifest
	if code != 0 || json.Unmarshal([]byte(out), &m) != nil || m.Schema != memory.ManifestSchema {
		t.Errorf("json dry run: code=%d out=%q", code, out[:min(len(out), 200)])
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--origin", "nope"}) }); code != 2 || !strings.Contains(stderr, "SLUG=PATH") {
		t.Errorf("bare --origin: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--format", "json"}) }); code != 1 {
		t.Errorf("--format json on the default apply: code=%d, want 1", code)
	}
	stale := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(stale, []byte(`{"schema":1,"configDir":"/elsewhere","items":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--manifest", stale}) }); code != 2 || !strings.Contains(stderr, "does not match this box") {
		t.Errorf("stale manifest: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := captureOutput(t, func() int { return cmdMemoryMigrate([]string{"--manifest", "/nope"}) }); code != 1 {
		t.Errorf("--manifest of a nonexistent manifest: code=%d, want 1", code)
	}
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"update", "--transcript", "/nonexistent", "--cwd", cfg, "--session", "s"})
	})
	if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
		t.Errorf("paused update: code=%d stderr=%q", code, stderr)
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
	bank := memory.HomeBankDir()
	target := filepath.Join(bank, "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	if err := os.MkdirAll(bank, 0o755); err != nil {
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

	code, out, _ = captureOutput(t, func() int { return cmdMemoryFmt(nil) })
	if code != 0 || !strings.Contains(out, "wrapped 1 file") {
		t.Fatalf("default fmt pass: code=%d out=%q, want 0 reporting one wrapped file", code, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		if len([]rune(l)) > memory.BankFmtMaxLine {
			t.Errorf("rewritten decisionLog.md still has an over-width line: %q", l)
		}
	}

	code, out, _ = captureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 0 || !strings.Contains(out, "clean") {
		t.Errorf("--check after wrapping: code=%d out=%q, want 0 clean", code, out)
	}

	if !strings.Contains(memoryUsage, "fmt") {
		t.Error("memoryUsage's plumbing note does not list `fmt`")
	}
}

// TestCmdMemoryFmtRefusedWhilePaused: fmt is a settle writer, gated the same as index/promote/update/dream.
func TestCmdMemoryFmtRefusedWhilePaused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"fmt"}) })
	if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
		t.Errorf("paused fmt: code=%d stderr=%q", code, stderr)
	}
}

// Phase 1 safety: a raised guard refuses every archive write (--apply, --restore --apply, --split --apply) at the
// memory layer, exit 1 with the notice, and the store is untouched; dry runs and reports stay ungated.
func TestMemoryArchiveWritesRefusedWhilePaused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := t.TempDir()
	dir := memory.NativeDir(repo)
	if err := os.MkdirAll(memory.NativeArchiveDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"topic.md":              "# T\nbody\n",
		memory.NativeIndexFile:  "- [T](topic.md) — t\n",
		"archive/native/old.md": "# O\nold\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := memory.SetMaintenance("test"); err != nil {
		t.Fatal(err)
	}
	writes := [][]string{
		{"--repo", repo, "topic", "--apply"},
		{"--repo", repo, "--restore", "old", "--apply"},
		{"--repo", repo, "--split", "0", "--apply"},
	}
	for _, args := range writes {
		code, _, stderr := captureOutput(t, func() int { return cmdMemoryArchive(args) })
		if code != 1 || !strings.Contains(stderr, "memory maintenance in progress (test)") {
			t.Errorf("paused archive %v: code=%d stderr=%q", args, code, stderr)
		}
	}
	for name, body := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != body {
			t.Errorf("%s changed under a paused guard: %q err=%v", name, data, err)
		}
	}
	reads := [][]string{
		{"--repo", repo, "topic"},
		{"--repo", repo, "--split", "0"},
		{"--repo", repo},
		{"--repo", repo, "--dupes"},
	}
	for _, args := range reads {
		if code, _, stderr := captureOutput(t, func() int { return cmdMemoryArchive(args) }); code != 0 {
			t.Errorf("read-only archive %v under a paused guard: code=%d stderr=%q", args, code, stderr)
		}
	}
}

// TestMemoryRecallRanksAndFormatsUnstampedHits: `memory recall` prints each hit "<id> — <title>",
// and an unstamped hit (no persisted id) prints "(<file basename>) — <title>" instead of a bare "-".
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
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
	code, out, stderr := captureOutput(t, func() int { return cmdMemory([]string{"recall", "--repo", repo, "stamped", "id-marked", "rule"}) })
	if code != 0 {
		t.Fatalf("memory recall: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "deadbeef — ") {
		t.Errorf("recall output missing the stamped id line: %q", out)
	}
	if !strings.Contains(out, "(activeContext.md) — ") {
		t.Errorf("recall output missing the unstamped-hit file-basename format: %q", out)
	}
	if strings.Contains(out, "\n- —") || strings.Contains(out, "\n — ") {
		t.Errorf("an unstamped hit printed the old bare-dash format: %q", out)
	}
}

// TestMemoryRecallNoQueryFails: an empty query is refused (exit 1), naming memoryUsage.
func TestMemoryRecallNoQueryFails(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"recall"}) })
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
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"recall", "--repo", repo, "--load", "3", "loadable", "widgets", "gadgets", "fact"})
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

// TestMemoryRememberUnknownTypeFails: the CLI surfaces memory.Remember's type-validation error.
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
	decisionLogOverCLI(t, filepath.Join(memory.Dir(repo), "decisionLog.md"), 80)
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
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Init(repo); err != nil {
		t.Fatal(err)
	}
	id, err := memory.Remember(repo, "feedback", "promote via the CLI", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := captureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, "--dry-run", id, "--to", "home"})
	})
	if code != 0 || !strings.Contains(out, "would move") {
		t.Fatalf("memory promote --dry-run: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(memory.HomeBankDir(), "conventions.md"))
	if strings.Contains(string(data), id) {
		t.Error("--dry-run via the CLI still wrote the home bank")
	}
	code, out, stderr = captureOutput(t, func() int {
		return cmdMemory([]string{"promote", "--repo", repo, id, "--to", "home"})
	})
	if code != 0 || !strings.Contains(out, "promoted ["+id+"]") {
		t.Fatalf("memory promote: code=%d out=%q stderr=%q", code, out, stderr)
	}
	data, err = os.ReadFile(filepath.Join(memory.HomeBankDir(), "conventions.md"))
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

// TestMemoryUpdateMissingTranscriptFailsNamingTheReference: cmdMemoryUpdate refuses to spawn a
// worker on nothing — exit 1, and the message points at skills/reflection/references/update.md.
func TestMemoryUpdateMissingTranscriptFailsNamingTheReference(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdMemory([]string{"update", "--cwd", t.TempDir(), "--session", "s"}) })
	if code != 1 {
		t.Fatalf("memory update with no --transcript: code=%d, want 1", code)
	}
	if !strings.Contains(stderr, "--transcript required") || !strings.Contains(stderr, "references/update.md") {
		t.Errorf("stderr missing the reference pointer: %q", stderr)
	}
}

// TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch: memoryUsage's help body names only the
// 7 public verbs; a private (plumbing) verb is absent from it but still dispatches when invoked.
func TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch(t *testing.T) {
	publicVerbs := []string{"recall", "remember", "init", "review", "promote", "update", "dream"}
	for _, v := range publicVerbs {
		if !strings.Contains(memoryUsage, "memory "+v) {
			t.Errorf("memoryUsage missing public verb %q: %q", v, memoryUsage)
		}
	}
	privateVerbs := []string{"search", "show", "index", "path", "digest", "fmt", "watch", "timeline", "archive", "migrate", "alias"}
	for _, v := range privateVerbs {
		if strings.Contains(memoryUsage, "memory "+v+" ") || strings.Contains(memoryUsage, "memory "+v+"\n") {
			t.Errorf("memoryUsage lists private verb %q as if public: %q", v, memoryUsage)
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
