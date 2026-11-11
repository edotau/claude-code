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
	return hosttest.CaptureOutput(t, func() int { return cmdMemory(args, nil) })
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
	memInit(t, repo)
	return repo
}

func memInit(t *testing.T, repo string) {
	t.Helper()
	if _, err := memory.Init(repo); err != nil {
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

// memFakeClaude puts a no-op claude binary on HARNESS_AGENT_BIN_DIR.
func memFakeClaude(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	memMust(t, os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("HARNESS_AGENT_BIN_DIR", bin)
}

// memTwoStores makes repos alpha and beta, each with file holding fmt.Sprintf(body, repo base name).
func memTwoStores(t *testing.T, file, body string) (a, b string) {
	t.Helper()
	a = hosttest.RepoAt(t, filepath.Join(t.TempDir(), "alpha"))
	b = hosttest.RepoAt(t, filepath.Join(t.TempDir(), "beta"))
	for _, repo := range []string{a, b} {
		dir, err := memory.EnsureRepoDir(repo)
		if err != nil {
			t.Fatal(err)
		}
		hosttest.WriteRel(t, dir, file, fmt.Sprintf(body, filepath.Base(repo)))
	}
	return a, b
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
		{"migrate verb gone", []string{"migrate", "--dry-run"}, 1, []string{"unknown subcommand"}},
		{"fmt bare --repo", []string{"fmt", "--repo", "/x"}, 2, nil},
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

func TestMemoryPathHonorsRepoFlag(t *testing.T) {
	for _, byStoreName := range []bool{false, true} {
		t.Run(fmt.Sprintf("storeName=%v", byStoreName), func(t *testing.T) {
			memCfg(t)
			repo, arg, want := hosttest.Repo(t), "", ""
			if byStoreName {
				dir, err := memory.EnsureRepoDir(repo)
				if err != nil {
					t.Fatal(err)
				}
				arg, want = filepath.Base(repo), dir
			} else {
				arg, want = repo, memory.Dir(repo)
			}
			code, out, stderr := memCLI(t, "path", "--repo", arg)
			if code != 0 || strings.TrimSpace(out) != want {
				t.Errorf("memory path --repo %s = %d %q (stderr %q), want %q", arg, code, out, stderr, want)
			}
		})
	}
}

// Init prints the embedded procedure on a fresh bank and names the "home" tier, never the pre-rename "global".
func TestMemoryInitAndIndexOutput(t *testing.T) {
	memCfg(t)
	repo := memFakeGitDir(t)
	out := memCLIOK(t, "init", "--repo", repo)
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

	out = memCLIOK(t, "index", "--repo", repo)
	if strings.Contains(strings.ToLower(out), "global") {
		t.Errorf("memory index output still names the pre-rename global tier: %q", out)
	}

	hosttest.WriteRel(t, memory.HomeBankDir(), "conventions.md", "# C\n\n- a rule\n")
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

// --check reports the over-width file without writing it; the default pass wraps it; the hint lists fmt.
func TestCmdMemoryFmt(t *testing.T) {
	memCfg(t)
	target := filepath.Join(memory.HomeBankDir(), "decisionLog.md")
	longLine := "- " + strings.Repeat("xxxxxxxxxx ", 14) + "xxxxxxxxxx"
	hosttest.WriteRel(t, memory.HomeBankDir(), "decisionLog.md", "# decisionLog\n\n"+longLine+"\n")

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
	repo := memoryBankRepo(t)
	code, out, stderr := memCLI(t, "remember", "--repo", repo, "--type", "project", "ship the recall verb")
	if code != 0 || !strings.Contains(out, "remembered [") {
		t.Fatalf("memory remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
	code, out, stderr = memCLI(t, "recall", "--repo", repo, "ship", "recall", "verb")
	if code != 0 || !strings.Contains(out, "ship the recall verb") {
		t.Fatalf("memory recall after remember: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestMemoryReview(t *testing.T) {
	// decisionLog entries make `memory review` report a cap problem.
	overCap := func(t *testing.T, repo string) {
		var b strings.Builder
		b.WriteString("# Decision Log\n")
		for i := 80; i >= 1; i-- {
			fmt.Fprintf(&b, "\n## decision %02d\n\nrationale topic%d choice%d\n", i, i, i)
		}
		hosttest.WriteRel(t, memory.Dir(repo), "decisionLog.md", b.String())
	}
	cases := []struct {
		name  string
		flag  string
		setup func(*testing.T, string)
		check func(*testing.T, string)
	}{
		{"doctor and stats blocks", "", nil, func(t *testing.T, out string) {
			if !strings.Contains(out, "memory doctor: no problems found") {
				t.Errorf("review output missing the doctor block: %q", out)
			}
			if !strings.Contains(out, memStatsHeader) {
				t.Errorf("review output missing the stats block: %q", out)
			}
		}},
		{"brief prints only the count", "--brief", overCap, func(t *testing.T, out string) {
			if !strings.Contains(out, "problem(s) found") {
				t.Errorf("--brief output missing the count line: %q", out)
			}
			if strings.Contains(out, "- decisionLog.md") {
				t.Errorf("--brief printed a per-line problem, want only the count: %q", out)
			}
		}},
		{"candidates procedure after stats", "--candidates", nil, func(t *testing.T, out string) {
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
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			memCfg(t)
			repo := memoryBankRepo(t)
			if c.setup != nil {
				c.setup(t, repo)
			}
			args := []string{"review", "--repo", repo}
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
	before := dirChecksum(memory.Dir(repo))
	out := memCLIOK(t, "dream", "--cwd", repo, "--dry-run")
	if !strings.Contains(out, "bank: "+memory.Dir(repo)) {
		t.Errorf("output missing the preview's bank line: %q", out)
	}
	if after := dirChecksum(memory.Dir(repo)); before != after {
		t.Errorf("--dry-run changed the bank directory: before=%s after=%s", before, after)
	}
}

// Without --cwd, `memory dream --dry-run` previews the bank of the dir it runs in.
func TestMemoryDreamDefaultsToCwd(t *testing.T) {
	memCfg(t)
	repo := hosttest.Repo(t) // an empty .git dir is no repo to git, so it would share the home bank
	memInit(t, repo)
	t.Chdir(repo)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdMemoryDream([]string{"--dry-run"}) })
	if code != 0 || !strings.Contains(out, "bank: "+memory.Dir(repo)) {
		t.Errorf("memory dream --dry-run in %s: code=%d out=%q stderr=%q, want its bank", repo, code, out, stderr)
	}
}

func TestMemoryPromoteCLIHomeMovesBlockAndDryRunWritesNothing(t *testing.T) {
	memCfg(t)
	repo := hosttest.Repo(t)
	memInit(t, repo)
	id, err := memory.Remember(repo, "feedback", "promote via the CLI", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := memCLI(t, "promote", "--repo", repo, "--dry-run", id, "--to", "home")
	if code != 0 || !strings.Contains(out, "would move") {
		t.Fatalf("memory promote --dry-run: code=%d out=%q stderr=%q", code, out, stderr)
	}
	home := filepath.Join(memory.HomeBankDir(), "conventions.md")
	if data, _ := os.ReadFile(home); strings.Contains(string(data), id) {
		t.Error("--dry-run via the CLI still wrote the home bank")
	}
	code, out, stderr = memCLI(t, "promote", "--repo", repo, id, "--to", "home")
	if code != 0 || !strings.Contains(out, "promoted ["+id+"]") {
		t.Fatalf("memory promote: code=%d out=%q stderr=%q", code, out, stderr)
	}
	if data, err := os.ReadFile(home); err != nil || !strings.Contains(string(data), id) {
		t.Fatalf("home bank missing the promoted id after the real run: %v %q", err, data)
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
	subs := memorySubcommands(nil)
	for _, v := range []string{"search", "show", "index", "path", "digest", "fmt", "watch", "timeline", "archive", "alias"} {
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
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir()) // a non-repo cwd: search rides the home bank
	body := "# D\n\n## 2026-10-01 — Zebra quota limiter choice <!-- id:abcd1234 -->\n\nzebra quota limiter fallback rationale\n"
	hosttest.WriteRel(t, memory.HomeBankDir(), "decisionLog.md", body)

	code, out, _ := memCLI(t, "show", "abcd1234")
	if code != 0 || !strings.Contains(out, "Zebra quota limiter choice") {
		t.Fatalf("memory show: code=%d out=%q", code, out)
	}
	code, out, _ = memCLI(t, "search", "zebra quota limiter")
	if code != 0 || !strings.Contains(out, "decisionLog.md") {
		t.Fatalf("memory search: code=%d out=%q", code, out)
	}
	data, err := os.ReadFile(memory.AccessLogPath(memory.HomeBankDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{memory.AccessShow, memory.AccessSearch} {
		if !strings.Contains(string(data), "abcd1234 -- "+reason) {
			t.Errorf("%s return not logged id-keyed: %q", reason, data)
		}
	}
}

// aliasFixture: isolated config with a cut-over store and two checkouts, each with its own store.
func aliasFixture(t *testing.T) (a, b, ka, kb string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg, _ := filepath.EvalSymlinks(memCfg(t))
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	hosttest.WriteRel(t, cfg, "state/harness/migrate/20260101-000000.journal.json", `{"phase":"complete","manifest":"/m/done.json"}`)
	memMust(t, memory.StoreRepo().Ensure())
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	a, b = hosttest.RepoAt(t, filepath.Join(parent, "alpha")), hosttest.RepoAt(t, filepath.Join(parent, "beta"))
	da, err := memory.EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := memory.EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	hosttest.WriteRel(t, db, "topic.md", "t\n")
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
	if code, _, _ := memCLI(t, "alias"); code == 0 {
		t.Errorf("Run alias without flags exited 0")
	}
}

func TestMemoryAliasDryRunThenApply(t *testing.T) {
	_, b, ka, kb := aliasFixture(t)
	db, _ := memory.RepoDir(b)
	alias := func(extra ...string) (int, string) {
		code, out, _ := hosttest.CaptureOutput(t, func() int {
			return cmdMemoryAlias(append([]string{"--repo", b, "--into", ka}, extra...))
		})
		return code, out
	}

	code, out := alias("--dry-run")
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, kb) {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("dry run removed the source store: %v", err)
	}
	code, out = alias()
	if code != 0 || !strings.HasPrefix(out, "applied:") {
		t.Fatalf("apply: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("source store survived apply: %v", err)
	}
	if got, _ := memory.RepoDir(b); filepath.Base(got) != ka {
		t.Errorf("RepoDir(beta) = %s, want key %s", got, ka)
	}
	if code, _ = alias(); code == 0 {
		t.Errorf("repeat apply exited 0, want non-zero")
	}
}

func TestMemoryCompletion(t *testing.T) {
	engine := memEngine()
	if got := engine.Candidates([]string{"memory", "al"}); !slices.Equal(got, []string{"alias"}) {
		t.Errorf("memory al* = %v, want [alias]", got)
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
		{"index flags", []string{"memory", "index", "--"}, []string{"--reembed", "--mint-only"}},
		{"recall flags", []string{"memory", "recall", "--"}, []string{"--explain"}},
		{"alias flags", []string{"memory", "alias", "--"}, []string{"--repo", "--into", "--dry-run"}},
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

// The install hint from claude.ClaudeBinary must reach the user.
func TestMemoryDigestReportsMissingClaudeBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "cfg"))
	code, _, stderr := memCLI(t, "digest", "--repo", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "install --tools=claude") {
		t.Errorf("memory digest without claude: code=%d stderr=%q, want 1 with the install hint", code, stderr)
	}
}

// An unstamped hit prints a content-derived ref, never a bare "-", and that ref resolves through `memory show`.
func TestMemoryRecallRanksAndFormatsUnstampedHits(t *testing.T) {
	memCfg(t)
	memMust(t, memory.StoreRepo().Ensure())
	repo := memoryBankRepo(t)
	hosttest.WriteRel(t, memory.Dir(repo), "conventions.md", "# Conventions\n\n- stamped id-marked rule <!-- id:deadbeef -->\n")
	// activeContext.md is never id-assigned, so this line stays unstamped (SearchBanks still indexes it).
	hosttest.WriteRel(t, memory.Dir(repo), "activeContext.md", "# Active Context\n\n- rule stamped id-marked never settled here\n")

	out := memCLIOK(t, "recall", "--repo", repo, "stamped", "id-marked", "rule")
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
		repo := memoryBankRepo(t)
		hosttest.WriteRel(t, memory.Dir(repo), "activeContext.md", "# Active Context\n\n- never settled widget rollout detail\n")
		out := memCLIOK(t, "recall", "--repo", repo, "--load", "1", "never", "settled", "widget", "rollout")
		if !strings.Contains(out, "never settled widget rollout detail") {
			t.Errorf("recall --load did not resolve the unstamped hit's body: %q", out)
		}
	})
	t.Run("appends bodies under the byte cap", func(t *testing.T) {
		memCfg(t)
		repo := memoryBankRepo(t)
		if _, err := memory.Remember(repo, "feedback", "loadable fact about widgets and gadgets", time.Now()); err != nil {
			t.Fatal(err)
		}
		out := memCLIOK(t, "recall", "--repo", repo, "--load", "3", "loadable", "widgets", "gadgets", "fact")
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
		hosttest.WriteRel(t, memory.HomeBankDir(), "decisionLog.md", before)
		code, out, stderr := memCLI(t, "fmt", "stray", "--check")
		if code != 2 {
			t.Errorf("fmt ignored stray before --check: exit=%d stdout=%q stderr=%q", code, out, stderr)
		}
		if !strings.Contains(stderr, `memory fmt: unexpected argument "stray"`) {
			t.Errorf("fmt missing positional usage error: %q", stderr)
		}
		after, err := os.ReadFile(filepath.Join(memory.HomeBankDir(), "decisionLog.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != before {
			t.Errorf("fmt wrote bank file despite invalid arguments: %q", after)
		}
	})
}

// TestSkillVerbsArePublicCLIVerbs checks the skill's verb table against memoryPage and memorySubcommands.
func TestSkillVerbsArePublicCLIVerbs(t *testing.T) {
	path := filepath.Join("..", "..", "skills", "reflection", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	start := strings.Index(body, "| Verb |")
	if start < 0 {
		t.Fatal("SKILL.md has no verb table (expected a \"| Verb |\" header row)")
	}
	table := body[start:]
	if end := strings.Index(table, "\n\n"); end >= 0 {
		table = table[:end]
	}
	verbRe := regexp.MustCompile("^`([a-z][a-z-]*)")
	subs := memorySubcommands(nil)
	seen := map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			continue
		}
		m := verbRe.FindStringSubmatch(strings.TrimSpace(cells[1])) // cells[0] is empty: the row starts with "|"
		if m == nil || seen[m[1]] {
			continue
		}
		verb := m[1]
		seen[verb] = true
		if _, ok := subs[verb]; !ok {
			t.Errorf("SKILL.md names unknown verb %q (not in memorySubcommands)", verb)
		} else if !helpPageHasCommand(memoryPage, verb) {
			t.Errorf("SKILL.md names verb %q that is not listed as public in memoryPage", verb)
		}
	}
	if len(seen) == 0 {
		t.Fatal("verb table names no verbs")
	}
}

func TestMemoryReportRegistersFreshRepoOrigin(t *testing.T) {
	memCfg(t)
	repo := hosttest.Repo(t)
	dir := memory.Dir(repo)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fresh repo store must not exist: %v", err)
	}
	input := filepath.Join(t.TempDir(), "report.json")
	memMust(t, os.WriteFile(input, []byte(`{"report":"# Review\n\nFresh repo report.\n","decisions":["Keep reports together."]}`), 0o600))
	for range 2 {
		stdout := memCLIOK(t, "report", "--repo", repo, "--label", "origin-regression", "--file", input, "--format", "json")
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
	for _, sub := range []string{"index", "digest", "review", "path", "init", "timeline", "watch", "catchup"} {
		code, out, stderr := memCLI(t, sub, "bogus")
		if want := `memory ` + sub + `: unexpected argument "bogus"`; code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("memory %s bogus: code=%d stdout=%q stderr=%q, want exit 2 with %q", sub, code, out, stderr, want)
		}
	}
	if after := treeNames(t, cfg); !slices.Equal(before, after) {
		t.Errorf("a refused stray argument wrote files: before=%v after=%v", before, after)
	}
}

// --repo <store name> targets that store from another checkout's cwd.
func TestMemoryIndexDigestRepoAcceptsStoreName(t *testing.T) {
	memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	memFakeClaude(t)
	a, b := hosttest.Repo(t), hosttest.Repo(t)
	dirA, err := memory.EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.EnsureRepoDir(b); err != nil {
		t.Fatal(err)
	}
	t.Chdir(b)
	name := filepath.Base(a)
	_, out, stderr := memCLI(t, "index", "--repo", name)
	if !strings.Contains(out, "indexed "+dirA) {
		t.Errorf("memory index --repo %s: stdout=%q stderr=%q, want it to index %s", name, out, stderr, dirA)
	}
	_, _, stderr = memCLI(t, "digest", "--repo", name)
	if !strings.Contains(stderr, filepath.Join(dirA, "archive")) {
		t.Errorf("memory digest --repo %s: stderr=%q, want it to read %s", name, stderr, dirA)
	}
}

// A --repo that is no path and no store fails and writes nothing.
func TestMemoryIndexDigestUnknownRepoNamesKnownStores(t *testing.T) {
	cfg := memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	memFakeClaude(t)
	repo := hosttest.Repo(t)
	if _, err := memory.EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	before := treeNames(t, cfg)
	for _, sub := range []string{"index", "digest"} {
		code, _, stderr := memCLI(t, sub, "--repo", "nosuch")
		if code != 1 || !strings.Contains(stderr, `"nosuch"`) || !strings.Contains(stderr, filepath.Base(repo)) {
			t.Errorf("memory %s --repo nosuch: code=%d stderr=%q, want exit 1 naming the known store %s", sub, code, stderr, filepath.Base(repo))
		}
	}
	if after := treeNames(t, cfg); !slices.Equal(before, after) {
		t.Errorf("an unknown --repo wrote files: before=%v after=%v", before, after)
	}
}

func TestMemoryReviewRepoStoreName(t *testing.T) {
	cfg := memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	a, b := memTwoStores(t, "activeContext.md", "# %s context\n")
	stale := time.Now().AddDate(0, 0, -memory.DefaultStaleDays-1)
	memMust(t, os.Chtimes(filepath.Join(memory.Dir(a), "activeContext.md"), stale, stale))
	t.Chdir(b)
	name := filepath.Base(memory.Dir(a))
	code, out, stderr := memCLI(t, "review", "--repo", name)
	if want := "local bank untouched since " + stale.Format("2006-01-02"); code != 0 || !strings.Contains(out, want) {
		t.Fatalf("memory review --repo %s: code=%d stdout=%q stderr=%q, want %q", name, code, out, stderr, want)
	}
	for _, args := range [][]string{{"review", "--repo", "nosuch"}, {"review", "--repo", "nosuch", "--notice"}} {
		checkMemoryUnknownRepo(t, cfg, args, name, filepath.Base(memory.Dir(b)))
	}
}

func TestMemoryTimelineRepoStoreName(t *testing.T) {
	cfg := memCfg(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	a, b := memTwoStores(t, "sessionHistory.md", "# Sessions\n\n## %s session\n")
	t.Chdir(b)
	name := filepath.Base(memory.Dir(a))
	code, out, stderr := memCLI(t, "timeline", "--repo", name)
	want := "-- sessionHistory --\n- " + filepath.Base(a) + " session\n"
	if code != 0 || out != want || stderr != "" {
		t.Fatalf("memory timeline --repo %s: code=%d stdout=%q stderr=%q, want %q", name, code, out, stderr, want)
	}
	checkMemoryUnknownRepo(t, cfg, []string{"timeline", "--repo", "nosuch"}, name, filepath.Base(memory.Dir(b)))
}

func checkMemoryUnknownRepo(t *testing.T, cfg string, args []string, knownStores ...string) {
	t.Helper()
	before := treeNames(t, cfg)
	code, out, stderr := memCLI(t, args...)
	if code != 1 || out != "" || !strings.Contains(stderr, `"nosuch"`) ||
		!strings.Contains(stderr, "known stores: "+strings.Join(knownStores, ", ")) {
		t.Errorf("memory %v: code=%d stdout=%q stderr=%q, want exit 1 naming known stores", args, code, out, stderr)
	}
	if after := treeNames(t, cfg); !slices.Equal(before, after) {
		t.Errorf("an unknown --repo wrote files: before=%v after=%v", before, after)
	}
}
