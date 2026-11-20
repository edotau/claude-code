package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const memStatsHeader = "== memory stats (receipts on disk) =="

func memMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// memCLIOK runs `memory <args>`, fails the test on a non-zero exit and returns stdout.
func memCLIOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, stderr := memCLI(t, args...)
	if code != 0 {
		t.Fatalf("memory %v: code=%d stdout=%q stderr=%q", args, code, out, stderr)
	}
	return out
}

// memCLI runs `memory <args>` and returns its exit code, stdout and stderr.
func memCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return hosttest.CaptureOutput(t, func() int { return cmdMemory(args) })
}

// memCfg points the config dir at a fresh temp dir and returns it.
func memCfg(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return cfg
}

// memoryBankRepo is a fresh repo-shaped dir with its bank scaffolded under the test's config dir.
func memoryBankRepo(t *testing.T) string {
	t.Helper()
	repo := memFakeGitDir(t)
	memInit(t)
	return repo
}

func memInit(t *testing.T) {
	t.Helper()
	if _, err := memory.Init(); err != nil {
		t.Fatal(err)
	}
}

// memFakeGitDir is an empty-.git directory: enough for memory.Init, not a repo to git.
func memFakeGitDir(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	memMust(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	return repo
}

func memEngine() helptext.Engine {
	return helptext.Engine{
		Tree:    func() map[string]*helptext.Node { return map[string]*helptext.Node{"memory": memorySpec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
}

func TestMemoryCLIRejections(t *testing.T) {
	memCfg(t)
	cases := []struct {
		name   string
		args   []string
		code   int
		stderr []string
	}{
		{"fmt bare --repo", []string{"fmt", "--repo", "/x"}, 2, nil},
		{"fmt --own outside the config dir", []string{"fmt", "--own"}, 1, []string{"is not the live config dir"}},
		{"update bare --repo", []string{"update", "--repo", "/x"}, 2, nil},
		{"dream bare --repo", []string{"dream", "--repo", "/x"}, 2, nil},
		{"show bare --repo", []string{"show", "--repo", "/x"}, 2, nil},
		{"promote without --to", []string{"promote", "abc123"}, 2, nil},
		{"promote two positionals", []string{"promote", "abc123", "extra", "--to", "home"}, 2, nil},
		{"remember unknown type", []string{"remember", "--type", "bogus", "a fact"}, 1, []string{"want"}},
		{"recall no query", []string{"recall"}, 1, []string{"memory recall: no query (usage: claude-code memory recall"}},
		{"search no query", []string{"search"}, 1, []string{"memory search: no query (usage: claude-code memory search"}},
		{"recall bad format", []string{"recall", "x", "--format", "xml"}, 2, []string{"memory recall: ", "format"}},
		{"search bad format", []string{"search", "x", "--format", "xml"}, 2, []string{"memory search: ", "format"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := memCLI(t, c.args...)
			if code != c.code {
				t.Errorf("cmdMemory(%v) = %d, want %d; stderr=%q", c.args, code, c.code, stderr)
			}
			for _, w := range c.stderr {
				if !strings.Contains(stderr, w) {
					t.Errorf("cmdMemory(%v) stderr=%q, want %q", c.args, stderr, w)
				}
			}
		})
	}
}

// Init prints the embedded procedure on a fresh bank and names the "home" tier, never the pre-rename "global".
func TestMemoryInitAndIndexOutput(t *testing.T) {
	memCfg(t)
	repo := memFakeGitDir(t)
	out := memCLIOK(t, "init")
	if !strings.Contains(out, "home:") {
		t.Errorf("memory init output missing the renamed home: line: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory init output still names the pre-rename global tier: %q", out)
	}
	ref, err := memory.Reference("init")
	if err != nil {
		t.Fatalf("memory.Reference(\"init\"): %v", err)
	}
	if !strings.Contains(out, ref) {
		t.Errorf("output missing the embedded init procedure: %q", out)
	}

	t.Chdir(repo)
	out = memCLIOK(t, "index")
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory index output still names the pre-rename global tier: %q", out)
	}

	hosttest.WriteRel(t, paths.MemoryStoreDir(), "conventions.md", "# C\n\n- a rule\n")
	catalog, err := memory.BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(catalog), "global") {
		t.Errorf("rendered catalog still names the pre-rename global tier: %q", catalog)
	}
}

// --check reports the over-width file without writing it; the default pass wraps it; the hint lists fmt.
func TestCmdMemoryFmtRejectsCheckWithOwn(t *testing.T) {
	memCfg(t)
	target := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", "# decisionLog\n\n"+longLine+"\n")
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt([]string{"--check", "--own"}) }); code != 2 {
		t.Fatalf("--check --own: code=%d, want 2", code)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), longLine) {
		t.Fatalf("--check --own wrote the bank: %q", data)
	}
}

func TestCmdMemoryFmt(t *testing.T) {
	memCfg(t)
	target := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", "# decisionLog\n\n"+longLine+"\n")

	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt([]string{"--check"}) })
	if code != 1 || !strings.Contains(out, target) {
		t.Fatalf("--check over a dirty bank: code=%d out=%q, want 1 naming %s", code, out, target)
	}
	if data, err := os.ReadFile(target); err != nil || !strings.Contains(string(data), longLine) {
		t.Fatalf("--check wrote the file: %q err=%v", data, err)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryFmt(nil) })
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
	if !strings.Contains(memoryPage.Hint, "fmt") {
		t.Error("memoryPage's plumbing hint does not list `fmt`")
	}
}

func TestMemoryRememberCLIRoundTripsThroughRecall(t *testing.T) {
	memCfg(t)
	memoryBankRepo(t)
	code, out, stderr := memCLI(t, "remember", "--type", "project", "ship the recall verb")
	if code != 0 || !strings.Contains(out, "remembered [") {
		t.Fatalf("memory remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
	code, out, stderr = memCLI(t, "recall", "ship", "recall", "verb")
	if code != 0 || !strings.Contains(out, "ship the recall verb") {
		t.Fatalf("memory recall after remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// overCapDecisions makes `memory review` report a cap problem: decisionLog entries past the cap.
func overCapDecisions(t *testing.T, repo string) {
	var b strings.Builder
	b.WriteString("# Decision Log\n")
	for i := 80; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## decision %02d\n\nrationale topic%d choice%d\n", i, i, i)
	}
	hosttest.WriteRel(t, paths.MemorySubDir(""), "decisionLog.md", b.String())
}

// checkReviewCandidates requires the review procedure to print after the stats block.
func checkReviewCandidates(t *testing.T, out string) {
	statsIdx := strings.Index(out, memStatsHeader)
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

func TestMemoryReview(t *testing.T) {
	cases := []struct {
		name  string
		flag  string
		setup func(*testing.T, string)
		check func(*testing.T, string)
	}{
		{"doctor and stats blocks", "", nil, func(t *testing.T, out string) {
			if !strings.Contains(out, "memory review: no problems found") {
				t.Errorf("review output missing the doctor block: %q", out)
			}
			if !strings.Contains(out, memStatsHeader) {
				t.Errorf("review output missing the stats block: %q", out)
			}
		}},
		{"brief prints only the count", "--brief", overCapDecisions, func(t *testing.T, out string) {
			if !strings.Contains(out, "problem(s) found") {
				t.Errorf("--brief output missing the count line: %q", out)
			}
			if strings.Contains(out, "- decisionLog.md") {
				t.Errorf("--brief printed a per-line problem, want only the count: %q", out)
			}
		}},
		{"candidates procedure after stats", "--candidates", nil, checkReviewCandidates},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			memCfg(t)
			repo := memoryBankRepo(t)
			if c.setup != nil {
				c.setup(t, repo)
			}
			t.Chdir(repo)
			args := []string{"review"}
			if c.flag != "" {
				args = append(args, c.flag)
			}
			out := memCLIOK(t, args...)
			c.check(t, out)
		})
	}
}

// dirChecksum sums every regular file's size+modtime under dir: detects any write without reading bytes.
func dirChecksum(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			fmt.Fprintf(&b, "%s|%d|%s\n", path, fi.Size(), fi.ModTime())
		}
		return nil
	})
	return b.String()
}

func TestMemoryDreamDryRunWritesNothing(t *testing.T) {
	memCfg(t)
	repo := memoryBankRepo(t)
	before := dirChecksum(paths.MemorySubDir(""))
	out := memCLIOK(t, "dream", "--cwd", repo, "--dry-run")
	if !strings.Contains(out, "bank: "+paths.MemorySubDir("")) {
		t.Errorf("output missing the preview's bank line: %q", out)
	}
	if after := dirChecksum(paths.MemorySubDir("")); before != after {
		t.Errorf("--dry-run changed the bank directory: before=%s after=%s", before, after)
	}
}

// Without --cwd, `memory dream --dry-run` previews the bank of the dir it runs in.
func TestMemoryDreamDefaultsToCwd(t *testing.T) {
	memCfg(t)
	repo := hosttest.Repo(t) // an empty .git dir is no repo to git, so it would share the home bank
	memInit(t)
	t.Chdir(repo)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemoryDream([]string{"--dry-run"}) })
	if code != 0 || !strings.Contains(out, "bank: "+paths.MemorySubDir("")) {
		t.Errorf("memory dream --dry-run in %s: code=%d out=%q stderr=%q, want its bank", repo, code, out, stderr)
	}
}

// Without --transcript (a live session) update prints the embedded procedure and exits 0.
func TestMemoryUpdateMissingTranscriptPrintsTheProcedure(t *testing.T) {
	memCfg(t)
	code, out, _ := memCLI(t, "update", "--cwd", t.TempDir(), "--session", "s")
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

func TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch(t *testing.T) {
	for _, v := range []string{"recall", "remember", "init", "review", "promote", "update", "dream"} {
		if !helpPageHasCommand(memoryPage, v) {
			t.Errorf("memoryPage missing public verb %q", v)
		}
	}
	subs := memorySubcommands()
	for _, v := range []string{"search", "show", "path", "fmt", "watch"} {
		if helpPageHasCommand(memoryPage, v) {
			t.Errorf("memoryPage lists private verb %q as if public", v)
		}
		if _, ok := subs[v]; !ok {
			t.Errorf("private verb %q is not registered in memorySubcommands (undispatchable)", v)
		}
	}
	memCfg(t)
	if code, _, stderr := memCLI(t, "path"); code != 0 {
		t.Errorf("private verb \"path\" failed to dispatch: code=%d stderr=%q", code, stderr)
	}
}

// Bare `memory update` resolves the live session's transcript from SessionIDEnv.
func TestMemoryUpdateLiveSession(t *testing.T) {
	cfg := memCfg(t)
	dir := t.TempDir()
	tr := filepath.Join(cfg, "projects", paths.ProjectSlug(dir), "live-1.jsonl")
	hosttest.WriteRel(t, filepath.Dir(tr), filepath.Base(tr), "{}\n")
	t.Setenv(paths.SessionIDEnv, "live-1")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || !strings.Contains(out, tr) {
		t.Errorf("live session: code=%d out=%q, want the transcript %s", code, out, tr)
	}
	t.Setenv(paths.SessionIDEnv, "")
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdMemoryUpdate([]string{"--dry-run", "--cwd", dir}) })
	if code != 0 || strings.Contains(out, "dry run") {
		t.Errorf("no session: code=%d out=%q, want the procedure, not a dry run", code, out)
	}
}

// `memory show` and `memory search` append id-keyed lines with their reasons to the owning bank's access log.
func TestMemoryShowAndSearchLogAccess(t *testing.T) {
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // a non-repo cwd: search rides the home bank
	body := "# D\n\n## 2026-10-01 — Zebra quota limiter choice <!-- id:abcd1234 -->\n\nzebra quota limiter fallback rationale\n"
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", body)

	code, out, _ := memCLI(t, "show", "abcd1234")
	if code != 0 || !strings.Contains(out, "Zebra quota limiter choice") {
		t.Fatalf("memory show: code=%d out=%q", code, out)
	}
	code, out, _ = memCLI(t, "search", "zebra quota limiter")
	if code != 0 || !strings.Contains(out, "decisionLog.md") {
		t.Fatalf("memory search: code=%d out=%q", code, out)
	}
	data, err := os.ReadFile(memory.AccessLogPath(paths.MemoryStoreDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{memory.AccessShow, memory.AccessSearch} {
		if !strings.Contains(string(data), "abcd1234 -- "+reason) {
			t.Errorf("%s return not logged id-keyed: %q", reason, data)
		}
	}
}

func TestMemoryCompletion(t *testing.T) {
	engine := memEngine()
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"review flags", []string{"memory", "review", "--"}, []string{"--candidates"}},
		{"update flags", []string{"memory", "update", "--"}, []string{"--dry-run", "--historical"}},
		{"dream flags", []string{"memory", "dream", "--"}, []string{"--dry-run"}},
		{"update catchup flags", []string{"memory", "update", "--"}, []string{"--catchup", "--skip"}},
		{"review timeline flags", []string{"memory", "review", "--"}, []string{"--timeline", "--n"}},
		{"index flags", []string{"memory", "index", "--"}, []string{"--reembed", "--mint-only"}},
		{"recall flags", []string{"memory", "recall", "--"}, []string{"--explain"}},
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

// Regression: memory search --format json must exit non-zero when stdout is unwritable.
func TestMemorySearchJSONWriteError(t *testing.T) {
	memCfg(t)
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-file"))
	if err != nil {
		t.Fatal(err)
	}
	memMust(t, f.Close())
	oldOut := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = oldOut }()
	if code := cmdMemorySearch([]string{"anyterm", "--format", "json"}); code == 0 {
		t.Errorf("memory search --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// Regression: memory show returns helptext.Fail's usage code (2) when no argument is given.
func TestMemoryShowFailReturnValue(t *testing.T) {
	memCfg(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemoryShow([]string{}) })
	if code != 2 {
		t.Errorf("memory show (no args) exit %d, want 2; stderr=%q", code, stderr)
	}
}

// An unstamped hit prints a content-derived ref, never a bare "-", and that ref resolves through `memory show`.
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	memCfg(t)
	memMust(t, memory.StoreRepo().Ensure())
	memoryBankRepo(t)
	hosttest.WriteRel(t, paths.MemorySubDir(""), "conventions.md", "# Conventions\n\n- stamped id-marked rule <!-- id:deadbeef -->\n")
	// activeContext.md is never id-assigned, so this line stays unstamped (SearchBanks still indexes it).
	hosttest.WriteRel(t, paths.MemorySubDir(""), "activeContext.md", "# Active Context\n\n- rule stamped id-marked never settled here\n")

	out := memCLIOK(t, "recall", "stamped", "id-marked", "rule")
	if !strings.Contains(out, "deadbeef — ") {
		t.Errorf("recall output missing the stamped id line: %q", out)
	}
	if strings.Contains(out, "\n- —") || strings.Contains(out, "\n — ") {
		t.Errorf("an unstamped hit printed the old bare-dash format: %q", out)
	}
	var contentRef string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if ref, _, ok := strings.Cut(line, " — "); ok && ref != "deadbeef" {
			contentRef = ref
		}
	}
	if !strings.HasPrefix(contentRef, "z") {
		t.Fatalf("recall output missing a content-derived ref for the unstamped hit: %q", out)
	}
	if _, block, err := memory.ShowBlock(contentRef); err != nil || !strings.Contains(block, "never settled here") {
		t.Errorf("memory show %s = %q, %v; want the unstamped block", contentRef, block, err)
	}
}

func TestMemoryRecallLoad(t *testing.T) {
	t.Run("resolves an unstamped hit by content ref", func(t *testing.T) {
		memCfg(t)
		memMust(t, memory.StoreRepo().Ensure())
		memoryBankRepo(t)
		hosttest.WriteRel(t, paths.MemorySubDir(""), "activeContext.md", "# Active Context\n\n- never settled widget rollout detail\n")
		out := memCLIOK(t, "recall", "--load", "1", "never", "settled", "widget", "rollout")
		if !strings.Contains(out, "never settled widget rollout detail") {
			t.Errorf("recall --load did not resolve the unstamped hit's body: %q", out)
		}
	})
	t.Run("appends bodies under the byte cap", func(t *testing.T) {
		memCfg(t)
		memoryBankRepo(t)
		if _, err := memory.Remember("feedback", "loadable fact about widgets and gadgets", time.Now()); err != nil {
			t.Fatal(err)
		}
		out := memCLIOK(t, "recall", "--load", "3", "loadable", "widgets", "gadgets", "fact")
		if !strings.Contains(out, "loadable fact about widgets and gadgets") {
			t.Errorf("recall --load output missing the loaded body: %q", out)
		}
		if len(out) > 20000 { // generous ceiling: --load must not dump the whole bank unbounded
			t.Errorf("recall --load output suspiciously large (%d bytes), want it capped near recallLoadCap", len(out))
		}
	})
}

// A stray positional is a usage error (exit 2) raised before --dry-run/--check can act on anything.
func TestMemoryStrayRejectedBeforeFlags(t *testing.T) {
	t.Run("dream", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "not-a-directory")
		memMust(t, os.WriteFile(cfg, []byte("blocked"), 0o600))
		// An invalid config dir stops a regressed forced dream before it reaches the model runner.
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		t.Setenv("CLAUDE_PROJECT_DIR", "")
		t.Chdir(t.TempDir())
		code, out, stderr := memCLI(t, "dream", "--force", "stray", "--dry-run")
		if code != 2 {
			t.Errorf("dream ignored stray before --dry-run: exit=%d stderr=%q", code, stderr)
		}
		if !strings.Contains(stderr, `memory dream: unexpected argument "stray"`) {
			t.Errorf("dream missing positional usage error: %q", stderr)
		}
		if out != "" {
			t.Errorf("dream ran despite invalid arguments: %q", out)
		}
	})
	t.Run("fmt", func(t *testing.T) {
		memCfg(t)
		t.Setenv("CLAUDE_PROJECT_DIR", "")
		t.Chdir(t.TempDir())
		before := "# Decision Log\n\n- " + strings.Repeat("longword ", 30) + "end\n"
		hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", before)
		code, out, stderr := memCLI(t, "fmt", "stray", "--check")
		if code != 2 {
			t.Errorf("fmt ignored stray before --check: exit=%d stdout=%q stderr=%q", code, out, stderr)
		}
		if !strings.Contains(stderr, `memory fmt: unexpected argument "stray"`) {
			t.Errorf("fmt missing positional usage error: %q", stderr)
		}
		after, err := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), "decisionLog.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != before {
			t.Errorf("fmt wrote bank file despite invalid arguments: %q", after)
		}
	})
}

// TestSkillVerbsArePublicCLIVerbs checks every verb memory-bank.md names is dispatchable via memorySubcommands.
func TestSkillVerbsArePublicCLIVerbs(t *testing.T) {
	path := filepath.Join("..", "..", "rules", "workflows", "memory-bank.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	start := strings.Index(body, "\n## Verbs\n")
	if start < 0 {
		t.Fatal("memory-bank.md has no \"## Verbs\" section")
	}
	section := body[start+1:]
	if end := strings.Index(section[3:], "\n## "); end >= 0 {
		section = section[:end+3]
	}
	verbRe := regexp.MustCompile("`([a-z][a-z-]*)[ `]")
	verbBulletRe := regexp.MustCompile(`^- (Read|Write|Settle|Move|Hook-spawned):`)
	subs := memorySubcommands()
	seen := map[string]bool{}
	for line := range strings.SplitSeq(section, "\n") {
		if !verbBulletRe.MatchString(line) { // only the grouped verb bullets; later bullets quote other commands
			continue
		}
		for _, m := range verbRe.FindAllStringSubmatch(line, -1) {
			verb := m[1]
			if seen[verb] {
				continue
			}
			seen[verb] = true
			if _, ok := subs[verb]; !ok {
				t.Errorf("memory-bank.md names unknown verb %q (not in memorySubcommands)", verb)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("Verbs section names no verbs")
	}
}

func helpPageHasCommand(page helptext.Page, name string) bool {
	for _, row := range page.Commands {
		if words := strings.Fields(row[0]); len(words) > 0 && words[0] == name {
			return true
		}
	}
	return false
}

// treeNames lists every path under root, so a test can prove a refused command created nothing.
func treeNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		names = append(names, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// Verbs that take no positional exit 2 on one and write nothing.
func TestMemoryNoArgVerbsRejectStray(t *testing.T) {
	cfg := memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(hosttest.Repo(t))
	before := treeNames(t, cfg)
	for _, sub := range []string{"index", "review", "path", "init", "watch"} {
		code, out, stderr := memCLI(t, sub, "bogus")
		if want := `memory ` + sub + `: unexpected argument "bogus"`; code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("memory %s bogus: code=%d stdout=%q stderr=%q, want exit 2 with %q", sub, code, out, stderr, want)
		}
	}
	if after := treeNames(t, cfg); !slices.Equal(before, after) {
		t.Errorf("a refused stray argument wrote files: before=%v after=%v", before, after)
	}
}

func TestMemorySandboxCLI(t *testing.T) {
	memCfg(t)
	repo := t.TempDir()
	hosttest.In(t, repo, "init", "--quiet")
	memInit(t)
	out := memCLIOK(t, "sandbox", "--empty", "--format", "json")
	var res memory.SandboxResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(res.Dir); err != nil {
			t.Error(err)
		}
	})
	if res.Dir == "" || res.Env["CLAUDE_CONFIG_DIR"] != res.Dir || len(res.Stores) != 1 {
		t.Fatalf("sandbox JSON: %+v", res)
	}
	for _, s := range res.Stores {
		if s.Files != 6 {
			t.Errorf("empty store: %+v", s)
		}
	}
	checkSandboxCompletion(t)
}

func checkSandboxCompletion(t *testing.T) {
	t.Helper()
	engine := memEngine()
	got := engine.Candidates([]string{"memory", "sandbox", "--"})
	for _, flag := range []string{"--empty", "--format"} {
		if !slices.Contains(got, flag) {
			t.Errorf("completion lacks %s: %v", flag, got)
		}
	}
}

func TestMemoryPathSubNameAndBareDir(t *testing.T) {
	memCfg(t)
	hosttest.Repo(t)
	for args, want := range map[string]string{
		"":                                     paths.MemorySubDir(""),
		"reports":                              paths.MemorySubDir("reports"),
		"reports claude-code--2026-01-02-x.md": paths.MemorySub("reports", "2026-01-02-x.md"),
		"knowledge claude-code--a.md":          paths.MemorySub("knowledge", "a.md"),
		"knowledge a.md":                       paths.MemorySub("knowledge", "a.md"),
		"knowledge a":                          paths.MemorySub("knowledge", "a.md"),
		"techdebt area":                        paths.MemorySub("techdebt", "area.md"),
	} {
		argv := append([]string{"path"}, strings.Fields(args)...)
		if code, out, errs := memCLI(t, argv...); code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("memory path %s = %d %q %q, want %q", args, code, out, errs, want)
		}
	}
	if code, _, _ := memCLI(t, "path", "bogus"); code != 2 {
		t.Errorf("memory path bogus code = %d, want 2", code)
	}
}

func TestMemoryUpdateCatchupDryRunWritesNothing(t *testing.T) {
	cfg := memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(hosttest.Repo(t))
	before := treeNames(t, cfg)
	if code, out, stderr := memCLI(t, "update", "--catchup", "--dry-run"); code != 0 {
		t.Fatalf("update --catchup --dry-run: code=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if after := treeNames(t, cfg); !slices.Equal(before, after) {
		t.Errorf("catchup dry-run wrote files: before=%v after=%v", before, after)
	}
}
