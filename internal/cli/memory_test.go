package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/mdlint"
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

func TestMemoryShadowRebuild(t *testing.T) {
	cfg := memCfg(t)
	for args, want := range map[string]int{"shadow": 2, "shadow --bogus": 2, "shadow --rebuild": 1} { // 1: no store yet
		if code, _, _ := memCLI(t, strings.Fields(args)...); code != want {
			t.Errorf("memory %s: code %d, want %d", args, code, want)
		}
	}
	memMust(t, os.MkdirAll(filepath.Join(cfg, "memory"), 0o700))
	memMust(t, os.WriteFile(filepath.Join(cfg, "memory", "decisionLog.md"), []byte("# D\n\n## 2026-01-01 — a\n"), 0o600))
	first, second := memCLIOK(t, "shadow", "--rebuild"), memCLIOK(t, "shadow", "--rebuild")
	if first != "shadow: 1 docs, 1 entries, 0 not byte-equal\n" || second != first {
		t.Fatalf("rebuild output %q then %q", first, second)
	}
	snaps, err := filepath.Glob(filepath.Join(cfg, "state", "backups", "memory-v0-*.db"))
	if _, serr := os.Stat(filepath.Join(cfg, "state", "memory.db")); serr != nil || err != nil || len(snaps) != 1 {
		t.Fatalf("db %v, snapshots %v (%v)", serr, snaps, err)
	}
}

func TestMemoryStore(t *testing.T) {
	memCfg(t)
	for _, args := range [][]string{{"store"}, {"store", "--truth", "db"}} {
		if out := memCLIOK(t, args...); out != "truth: db\n" {
			t.Fatalf("%v = %q", args, out)
		}
	}
	code, _, stderr := memCLI(t, "store", "--truth", "files")
	if code != 1 || !strings.Contains(stderr, "retired") {
		t.Fatalf("set files: code %d stderr %q, want a refusal", code, stderr)
	}
	for _, bad := range []string{"bogus", ""} {
		if code, _, stderr := memCLI(t, "store", "--truth="+bad); code != 2 || !strings.Contains(stderr, "want db") {
			t.Fatalf("bad value %q: code %d stderr %q", bad, code, stderr)
		}
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
		{"show is retired", []string{"show", "abcd1234"}, 2, []string{`unknown subcommand "show"`}},
		{"recall is retired", []string{"recall", "x"}, 2, []string{`unknown subcommand "recall"`}},
		{"promote without --to", []string{"promote", "abc123"}, 2, nil},
		{"promote two positionals", []string{"promote", "abc123", "extra", "--to", "home"}, 2, nil},
		{"remember unknown type", []string{"remember", "--type", "bogus", "a fact"}, 1, []string{"want"}},
		{"search no query", []string{"search"}, 1, []string{"memory search: no query (usage: claude-code memory search"}},
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
	ref := assets.MemoryInitProcedure()
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
		if len([]rune(l)) > mdlint.Width {
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
	code, out, stderr = memCLI(t, "search", "ship", "recall", "verb")
	if code != 0 || !strings.Contains(out, "ship the recall verb") {
		t.Fatalf("memory search after remember: code=%d out=%q stderr=%q", code, out, stderr)
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
	ref := assets.MemoryUpdateProcedure()
	if !strings.Contains(out, ref) {
		t.Errorf("stdout missing the embedded update procedure: %q", out)
	}
}

func TestMemoryUsageListsOnlyPublicVerbsAndPrivateStillDispatch(t *testing.T) {
	for _, v := range []string{"search", "remember", "init", "review", "feedback", "promote", "update", "dream"} {
		if !helpPageHasCommand(memoryPage, v) {
			t.Errorf("memoryPage missing public verb %q", v)
		}
	}
	subs := memorySubcommands()
	for _, v := range []string{"path", "fmt", "watch"} {
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

// `memory search --id` and `memory search` append id-keyed lines with their reasons to the owning bank's access log.
func TestMemoryShowAndSearchLogAccess(t *testing.T) {
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // a non-repo cwd: search rides the home bank
	body := "# D\n\n## 2026-10-01 — Zebra quota limiter choice <!-- id:abcd1234 -->\n\nzebra quota limiter fallback rationale\n"
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", body)

	code, out, _ := memCLI(t, "search", "--id", "abcd1234")
	if code != 0 || !strings.Contains(out, "Zebra quota limiter choice") {
		t.Fatalf("memory search --id: code=%d out=%q", code, out)
	}
	code, out, _ = memCLI(t, "search", "zebra quota limiter")
	if code != 0 || !strings.Contains(out, "decisionLog.md") {
		t.Fatalf("memory search: code=%d out=%q", code, out)
	}
	data, err := os.ReadFile(filepath.Join(memory.AccessStateDir(), "home.log"))
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
		{"review flags", []string{"memory", "review", "--"}, []string{"--stale-days", "--brief", "--notice"}},
		{"feedback verbs", []string{"memory", "feedback", ""}, []string{"list", "show", "add", "approve", "close"}},
		{"feedback list flags", []string{"memory", "feedback", "list", "--"}, []string{"--rank"}},
		{"update flags", []string{"memory", "update", "--"}, []string{"--dry-run", "--historical"}},
		{"dream flags", []string{"memory", "dream", "--"}, []string{"--dry-run"}},
		{"update catchup flags", []string{"memory", "update", "--"}, []string{"--catchup", "--skip"}},
		{"review timeline flags", []string{"memory", "review", "--"}, []string{"--timeline"}},
		{"index flags", []string{"memory", "index", "--"}, []string{"--reembed", "--mint-only"}},
		{"search open flag", []string{"memory", "search", "--"}, []string{"--open", "--explain"}},
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

// An unstamped hit prints a content-derived ref, never a bare "-", and that ref resolves through `memory search --id`.
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	memCfg(t)
	memMust(t, memory.StoreRepo().Ensure())
	memoryBankRepo(t)
	hosttest.WriteRel(t, paths.MemorySubDir(""), "conventions.md", "# Conventions\n\n- stamped id-marked rule <!-- id:deadbeef -->\n")
	// activeContext.md is never id-assigned, so this line stays unstamped (SearchBanks still indexes it).
	hosttest.WriteRel(t, paths.MemorySubDir(""), "activeContext.md", "# Active Context\n\n- rule stamped id-marked never settled here\n")

	out := memCLIOK(t, "search", "--open", "0", "stamped", "id-marked", "rule")
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
		t.Errorf("memory.ShowBlock(%s) = %q, %v; want the unstamped block", contentRef, block, err)
	}
}

func TestMemoryRecallLoad(t *testing.T) {
	t.Run("resolves an unstamped hit by content ref", func(t *testing.T) {
		memCfg(t)
		memMust(t, memory.StoreRepo().Ensure())
		memoryBankRepo(t)
		hosttest.WriteRel(t, paths.MemorySubDir(""), "activeContext.md", "# Active Context\n\n- never settled widget rollout detail\n")
		out := memCLIOK(t, "search", "--open", "1", "never", "settled", "widget", "rollout")
		if !strings.Contains(out, "never settled widget rollout detail") {
			t.Errorf("search --open did not resolve the unstamped hit's body: %q", out)
		}
	})
	t.Run("appends bodies under the byte cap", func(t *testing.T) {
		memCfg(t)
		memoryBankRepo(t)
		if _, err := memory.Remember("feedback", "loadable fact about widgets and gadgets", time.Now()); err != nil {
			t.Fatal(err)
		}
		out := memCLIOK(t, "search", "--open", "3", "loadable", "widgets", "gadgets", "fact")
		if !strings.Contains(out, "loadable fact about widgets and gadgets") {
			t.Errorf("search --open output missing the loaded body: %q", out)
		}
		if len(out) > 20000 { // generous ceiling: --load must not dump the whole bank unbounded
			t.Errorf("search --open output suspiciously large (%d bytes), want it capped near recallLoadCap", len(out))
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

func TestRetiredMemoryVerbsExitUsage(t *testing.T) {
	memCfg(t)
	for _, verb := range []string{"import", "fetch"} {
		code, _, stderr := memCLI(t, verb, "--from", t.TempDir())
		if code != 2 || !strings.Contains(stderr, "memory "+verb+": retired") || !strings.Contains(stderr, "memory syncs with `claude-code memory sync`") {
			t.Errorf("memory %s: code %d, stderr %q; want 2 naming memory sync", verb, code, stderr)
		}
	}
}

func TestMemorySyncOffExitsUsage(t *testing.T) {
	memCfg(t)
	t.Setenv("NETWORK_MEMORY", "")
	if code, _, stderr := memCLI(t, "sync", "--host", "peer.example"); code != 2 || !strings.Contains(stderr, memory.SyncOffMsg) {
		t.Fatalf("code %d, stderr %q; want 2 and the opt-in hint", code, stderr)
	}
}

func TestMemorySyncServeIsHiddenAndTyped(t *testing.T) {
	memCfg(t)
	if _, _, help := memCLI(t, "sync", "-h"); strings.Contains(help, "serve") || strings.Contains(help, "since") {
		t.Errorf("help lists the hidden serve flags: %q", help)
	}
	for _, args := range [][]string{{"--serve", "bogus"}, {"--serve", "export", "--since", "a=1;rm -rf /"}, {"--serve", "import", "--since", "a=1"}} {
		if code, _, _ := memCLI(t, append([]string{"sync"}, args...)...); code != 2 {
			t.Errorf("sync %v: code %d, want 2", args, code)
		}
	}
}

// `memory search --open` reproduces the retired `recall --load` output, exit code and stderr (goldens from main).
func TestMemorySearchOpenParity(t *testing.T) {
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	dir, _ := os.Getwd()
	t.Chdir(t.TempDir())
	seedMemoryOpenFixtures(t, dir)
	for _, tc := range []struct {
		name string // golden basename: .golden is stdout, .stderr and .code sit beside it
		args []string
	}{
		{"load3", []string{"search", "--open", "3", "zebrafish", "kestrel"}},
		{"nomatch", []string{"search", "--open", "2", "qqqqxyz"}},
		{"json", []string{"search", "--open", "3", "--format", "json", "zebrafish", "kestrel"}},
		{"nomatchjson", []string{"search", "--open", "2", "--format", "json", "qqqqxyz"}},
		{"explicitk", []string{"search", "-k", "2", "--open", "2", "zebrafish", "kestrel"}},
		{"manyopen", []string{"search", "--open", "12", "ibis", "colony"}}, // 12 hits: --open defaults to -k 3
		{"manyk", []string{"search", "-k", "10", "--open", "12", "ibis", "colony"}},
		{"manysearch", []string{"search", "ibis", "colony"}}, // 12 hits: search defaults to -k 8
		{"manysearchjson", []string{"search", "--format", "json", "ibis", "colony"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(ext string) string {
				b, err := os.ReadFile(filepath.Join(dir, "testdata/memory_open", tc.name+ext))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			before := memoryAccessLogBytes(t)
			code, out, stderr := memCLI(t, tc.args...)
			if after := memoryAccessLogBytes(t); slices.Contains(tc.args, "--open") && after != before {
				t.Errorf("%v wrote access logs; recall --load wrote none", tc.args)
			}
			if want := strings.TrimSpace(read(".code")); strconv.Itoa(code) != want {
				t.Errorf("%v exit = %d, want %s", tc.args, code, want)
			}
			if want := read(".golden"); out != want {
				t.Errorf("%v stdout = %d bytes, want the golden's %d bytes", tc.args, len(out), len(want))
			}
			if want := read(".stderr"); stderr != want {
				t.Errorf("%v stderr = %q, want %q", tc.args, stderr, want)
			}
		})
	}
}

// search rejects --id, --open and --explain in any pair, and --open below 1, with exit 2 naming the flags.
func TestMemorySearchFlagConflicts(t *testing.T) {
	memCfg(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"search", "--id", "abc12345", "--open", "1"}, []string{"--id", "--open"}},
		{[]string{"search", "--id", "abc12345", "--explain"}, []string{"--id", "--explain"}},
		{[]string{"search", "--explain", "--open", "1", "q"}, []string{"--open", "--explain"}},
		{[]string{"search", "--explain", "--format", "json", "q"}, []string{"--explain", "json"}},
	} {
		code, _, stderr := memCLI(t, tc.args...)
		if code != 2 {
			t.Errorf("%v exit = %d, want 2", tc.args, code)
		}
		for _, w := range tc.want {
			if !strings.Contains(stderr, w) {
				t.Errorf("%v stderr %q lacks %q", tc.args, stderr, w)
			}
		}
	}
}

// memoryAccessLogBytes totals the access logs' sizes so a case can prove it left them alone.
func memoryAccessLogBytes(t *testing.T) (n int64) {
	t.Helper()
	logs, _ := filepath.Glob(filepath.Join(memory.AccessStateDir(), "*.log"))
	for _, l := range logs {
		if fi, err := os.Stat(l); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// seedMemoryOpenFixtures copies the golden set's two fixture files from dir/testdata into the store.
func seedMemoryOpenFixtures(t *testing.T, dir string) {
	t.Helper()
	for _, f := range []string{"conventions.md", "decisionLog.md"} {
		fixture, err := os.ReadFile(filepath.Join(dir, "testdata/memory_open", f))
		if err != nil {
			t.Fatal(err)
		}
		hosttest.WriteRel(t, paths.MemoryStoreDir(), f, string(fixture))
	}
}

// feedbackSeed copies the golden set's seed tree into the store (store/) and the config dir (config/).
func feedbackSeed(t *testing.T, dir string) {
	t.Helper()
	for sub, root := range map[string]string{"store": paths.MemoryStoreDir(), "config": paths.ConfigDir()} {
		base := filepath.Join(dir, "testdata/memory_feedback/seed", sub)
		memMust(t, filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, p)
			hosttest.WriteRel(t, root, rel, string(body))
			return nil
		}))
	}
}

var (
	feedbackCwdRE = regexp.MustCompile(`"cwd":"[^"]*"`)
	// feedbackOldToNew rewrites the retired flag wording in a main golden to the verbs that replaced it.
	feedbackOldToNew = strings.NewReplacer("`memory review --candidate <id>`", "`memory feedback show <id>`",
		"--enforced wants", "close wants", "--enforced: ", "close: ", "(--approve first)", "(approve it first)",
		"the owner's --approve sets", "the owner's approval sets")
	feedbackApproveRE = regexp.MustCompile(`claude-code memory review --candidate (\S+) --approve`)
	feedbackCloseRE   = regexp.MustCompile(`claude-code memory review --candidate (\S+) --enforced <sha\|file:line>`)
)

// feedbackGolden reads one golden of main's `memory review` and rewrites its wording for verb ("" keeps the prefix).
func feedbackGolden(t *testing.T, dir, name, ext, verb string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "testdata/memory_feedback", name+ext))
	if err != nil {
		t.Fatal(err)
	}
	s := feedbackOldToNew.Replace(string(b))
	s = feedbackApproveRE.ReplaceAllString(s, "claude-code memory feedback approve $1")
	s = feedbackCloseRE.ReplaceAllString(s, "claude-code memory feedback close $1 <sha|file:line>")
	if verb != "" {
		s = strings.Replace(s, "claude-code: memory review: ", "claude-code: memory feedback "+verb+": ", 1)
	}
	return s
}

// feedbackCase is one golden: pre are space-separated `memory feedback` calls run before the captured args.
type feedbackCase struct {
	name, verb string
	pre        []string
	args       []string
}

func feedbackParityCases() []feedbackCase {
	const pass = "approve fc-b"
	candidateC := `{"id":"fc-c","evidence":["feedback/feedback-a.md","notes/note-c.md"],"repeats":2,"change":"Add a rule.",` +
		`"target":"rules/x.md","kind":"prose","size":"small"}`
	candidateA := `{"id":"fc-a","evidence":["feedback/feedback-a.md","notes/note-c.md"],"repeats":1,"change":"Add a hook.",` +
		`"target":"unknown","kind":"hook","size":"small"}`
	return []feedbackCase{
		{"review_plain", "", nil, []string{"review"}},
		{"review_brief", "", nil, []string{"review", "--brief"}},
		{"review_notice", "", nil, []string{"review", "--notice"}},
		{"review_stale", "", nil, []string{"review", "--stale-days", "30"}},
		{"review_timeline", "", nil, []string{"review", "--timeline", "5"}},
		{"review_open_approved", "", []string{pass}, []string{"review"}},
		{"show_proposed", "show", nil, []string{"feedback", "show", "fc-a"}},
		{"show_approved", "show", []string{pass}, []string{"feedback", "show", "fc-b"}},
		{"show_enforced", "show", []string{pass, "close fc-b rules/x.md:2"}, []string{"feedback", "show", "fc-b"}},
		{"show_missing", "show", nil, []string{"feedback", "show", "fc-zz"}},
		{"approve_ok", "approve", nil, []string{"feedback", "approve", "fc-b"}},
		{"approve_twice", "approve", []string{pass}, []string{"feedback", "approve", "fc-b"}},
		{"approve_missing", "approve", nil, []string{"feedback", "approve", "fc-zz"}},
		{"close_ok", "close", []string{pass}, []string{"feedback", "close", "fc-b", "rules/x.md:2"}},
		{"close_unapproved", "close", nil, []string{"feedback", "close", "fc-b", "rules/x.md:2"}},
		{"close_badline", "close", []string{pass}, []string{"feedback", "close", "fc-b", "rules/x.md:9"}},
		{"close_badref", "close", []string{pass}, []string{"feedback", "close", "fc-b", "nonsense"}},
		{"close_nofile", "close", []string{pass}, []string{"feedback", "close", "fc-b", "nope.md:1"}},
		{"close_nocommit", "close", []string{pass}, []string{"feedback", "close", "fc-b", "deadbeef0"}},
		{"add_ok", "add", nil, []string{"feedback", "add", candidateC}},
		{"add_dup", "add", nil, []string{"feedback", "add", candidateA}},
		{"add_empty", "add", nil, []string{"feedback", "add", "{}"}},
		{"add_status", "add", nil, []string{"feedback", "add",
			strings.Replace(candidateC, `"id"`, `"status":"approved","id"`, 1)}},
	}
}

// The feedback verbs print what main's `memory review --candidate|--approve|--enforced|--propose` printed.
func TestMemoryFeedbackParity(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("CLAUDE_MEMORY_DREAM_CHILD", "")
	dir, _ := os.Getwd()
	cases := feedbackParityCases()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			memCfg(t)
			memInit(t)
			t.Chdir(t.TempDir())
			feedbackSeed(t, dir)
			for _, pre := range tc.pre {
				memCLIOK(t, append([]string{"feedback"}, strings.Fields(pre)...)...)
			}
			code, out, stderr := memCLI(t, tc.args...)
			out = feedbackCwdRE.ReplaceAllString(out, `"cwd":"@CWD@"`)
			want, wantErr := feedbackGolden(t, dir, tc.name, ".golden", ""), feedbackGolden(t, dir, tc.name, ".stderr", tc.verb)
			if wantCode := strings.TrimSpace(feedbackGolden(t, dir, tc.name, ".code", "")); strconv.Itoa(code) != wantCode {
				t.Errorf("%v exit = %d, want %s", tc.args, code, wantCode)
			}
			if out != want {
				t.Errorf("%v stdout:\n%s\nwant:\n%s", tc.args, out, want)
			}
			if stderr != wantErr {
				t.Errorf("%v stderr = %q, want %q", tc.args, stderr, wantErr)
			}
		})
	}
}

// `feedback list` prints the open-candidates block of plain review, --rank appends the procedure, none says so.
func TestMemoryFeedbackList(t *testing.T) {
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	dir, _ := os.Getwd()
	memCfg(t)
	memInit(t)
	t.Chdir(t.TempDir())
	if out := memCLIOK(t, "feedback", "list"); out != "no open feedback candidates\n" {
		t.Errorf("empty list = %q", out)
	}
	feedbackSeed(t, dir)
	review := feedbackGolden(t, dir, "review_plain", ".golden", "")
	block := review[strings.Index(review, "open feedback candidates"):]
	out := memCLIOK(t, "feedback", "list")
	if out != block {
		t.Errorf("list = %q, want review's block %q", out, block)
	}
	ref := assets.MemoryReviewProcedure()
	if ranked := memCLIOK(t, "feedback", "list", "--rank"); ranked != block+"\n"+ref+"\n" {
		t.Errorf("list --rank = %q, want the block, a blank line and the procedure", ranked)
	}
}

// Each retired review flag exits 2 naming its replacement, and a malformed feedback call exits 2 with usage.
func TestMemoryFeedbackRetiredAndUsage(t *testing.T) {
	memCfg(t)
	cases := []struct {
		name string
		args []string
		want string
		not  string // must be absent from stderr
	}{
		{"candidate", []string{"review", "--candidate", "fc-a"}, "claude-code memory feedback show <id>", ""},
		{"approve", []string{"review", "--candidate", "fc-a", "--approve"}, "claude-code memory feedback approve <id>", ""},
		{"enforced", []string{"review", "--enforced=abc1234"}, "claude-code memory feedback close <id> <sha|file:line>", ""},
		{"propose", []string{"review", "--propose", "{}"}, "claude-code memory feedback add '<json>'", ""},
		{"candidates", []string{"review", "--candidates"}, "claude-code memory feedback list --rank", ""},
		{"n", []string{"review", "-n", "5"}, "claude-code memory review --timeline N", ""},
		{"candidate+enforced closes", []string{"review", "--candidate", "fc-a", "--enforced", "rules/x.md:2"}, "memory feedback close <id>", "feedback show"},
		{"approve=false is not approve", []string{"review", "--candidate", "fc-a", "--approve=false"}, "memory feedback show <id>", "feedback approve"},
		{"timeline beats candidate", []string{"review", "--timeline", "--n", "5", "--candidate", "fc-a"}, "memory review --timeline N", "feedback show"},
		{"n loses to candidate", []string{"review", "--n", "5", "--candidate", "fc-a"}, "memory feedback show <id>", "--timeline N"},
		{"unknown verb", []string{"feedback", "open"}, `unknown verb "open"`, ""},
		{"show no id", []string{"feedback", "show"}, "usage: claude-code memory feedback show", ""},
		{"show blank id", []string{"feedback", "show", " "}, "usage: claude-code memory feedback show", ""},
		{"close one arg", []string{"feedback", "close", "fc-a"}, "usage: claude-code memory feedback close", ""},
		{"list stray", []string{"feedback", "list", "fc-a"}, "usage: claude-code memory feedback list", ""},
		{"add two", []string{"feedback", "add", "{}", "{}"}, "usage: claude-code memory feedback add", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, stderr := memCLI(t, tc.args...)
			if code != 2 || out != "" || !strings.Contains(stderr, tc.want) || (tc.not != "" && strings.Contains(stderr, tc.not)) {
				t.Errorf("memory %v: code=%d stdout=%q stderr=%q, want exit 2 naming %q", tc.args, code, out, stderr, tc.want)
			}
		})
	}
}

// A false or empty retired flag counts as not given (the health report runs); a bare --timeline keeps its default.
func TestMemoryReviewFalseRetiredFlagsRun(t *testing.T) {
	memCfg(t)
	for _, args := range [][]string{
		{"review", "--approve=false"},
		{"review", "--candidates=false"},
		{"review", "--enforced="},
		{"review", "--timeline"},
		{"review", "--timeline", "--brief"},
	} {
		code, _, stderr := memCLI(t, args...)
		if code == 2 || strings.Contains(stderr, "is gone") {
			t.Errorf("memory %v: code=%d stderr=%q, want it to run", args, code, stderr)
		}
	}
}

// The search verb flushes recall timing itself, to stderr only, in text and JSON.
func TestMemorySearchRecallTiming(t *testing.T) {
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir())
	hosttest.WriteRel(t, paths.MemoryStoreDir(), "decisionLog.md", "# D\n\n## 2026-10-20 — Zebrafish pipeline timing choice <!-- id:bbbb0001 -->\n\nzebrafish pipeline timing notes rationale\n")
	for _, args := range [][]string{{"search", "zebrafish pipeline"}, {"search", "--format", "json", "zebrafish pipeline"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("CLAUDE_RECALL_TIMING", "1")
			code, out, stderr := memCLI(t, args...)
			if code != 0 || !strings.Contains(out, "decisionLog") {
				t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
			}
			if strings.Contains(out, "recall-timing") {
				t.Fatalf("timing leaked to stdout: %q", out)
			}
			if n := strings.Count(stderr, "recall-timing"); n != 1 {
				t.Fatalf("recall-timing lines = %d: %q", n, stderr)
			}
			t.Setenv("CLAUDE_RECALL_TIMING", "")
			_, out2, stderr2 := memCLI(t, args...)
			if strings.Contains(out2+stderr2, "recall-timing") {
				t.Fatalf("disabled run printed timing: %q %q", out2, stderr2)
			}
		})
	}
}

func TestMemoryUpgradeIsGoneAndExitsTwo(t *testing.T) {
	memCfg(t)
	code, _, stderr := memCLI(t, "upgrade")
	if code != 2 || !strings.Contains(stderr, "the store is the database now") {
		t.Errorf("memory upgrade: code=%d stderr=%q, want exit 2 naming the database", code, stderr)
	}
	if helpPageHasCommand(memoryPage, "upgrade") || strings.Contains(memoryPage.Hint, "upgrade") {
		t.Error("memoryPage still lists upgrade")
	}
}

func TestMemoryServeImportReportsBusyBeforeReading(t *testing.T) {
	memCfg(t)
	memInit(t)
	old := memory.BankLockWait
	memory.BankLockWait = 20 * time.Millisecond
	t.Cleanup(func() { memory.BankLockWait = old })
	err := memory.WithBankLock(time.Second, func() error {
		code, _, stderr := memCLI(t, "sync", "--serve", "import")
		if code != memory.ImportBusyExit || !strings.Contains(stderr, "busy") {
			t.Errorf("busy serve: exit %d, stderr %q", code, stderr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
