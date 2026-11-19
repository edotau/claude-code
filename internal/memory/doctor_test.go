package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths"
)

func TestHookSignalsReport(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	var report bytes.Buffer
	writeHookSignals(&report, now)
	if report.Len() != 0 {
		t.Fatal("signals printed without telemetry")
	}
	dir := filepath.Join(paths.StateDir(), "hooks")
	rows := signalTestRows()
	for day := range 3 {
		file := "telemetry-" + now.AddDate(0, 0, -day).Format("20060102") + ".jsonl"
		write(t, filepath.Join(dir, file), rows)
	}
	file := filepath.Join(dir, "telemetry-20261118.jsonl")
	write(t, file, rows+"not-json\n"+`{"module":false}`+"\n"+`{"outcome":"dropped"}`+"\n")
	write(t, filepath.Join(dir, "telemetry-20261104.jsonl"), rows)
	writeHookSignals(&report, now)
	const want = `signals (since 2026-11-05):
  block: guard-a (gate) 72 (24.0%)
  block: guard-b (gate) 36 (12.0%)
  ask: confirm-a (gate) 27 (9.0%)
  ask: confirm-b (gate) 9 (3.0%)
  hand-backs: held 12, refused 12, passed 21; top refused: missing-id (handoff) 9
  rewrite: 15
  dropped: >=1 (cap markers)
`
	if got := report.String(); got != want {
		t.Errorf("signals output differs from allowlisted report:\n got: %q\nwant: %q", got, want)
	}
}

func signalTestRows() string {
	var rows strings.Builder
	for _, row := range []struct {
		module, rule, verdict string
		count                 int
	}{
		{"gate", "guard-a", "block", 24}, {"gate", "guard-b", "block", 12}, {"gate", "guard-c", "block", 2},
		{"gate", "confirm-a", "ask", 9}, {"gate", "confirm-b", "ask", 3}, {"gate", "", "", 50},
		{"handoff", "running", "held", 4}, {"handoff", "missing-id", "refused", 3},
		{"handoff", "duplicate", "refused", 1}, {"handoff", "receipt", "passed", 7},
		{"sanitizer", "rewrite-label", "rewrite", 5},
	} {
		line := fmt.Sprintf(`{"module":%q,"rule":%q,"verdict":%q,"outcome":"deny",`+
			`"t":"fake-time","event":"fake-event","ms":123456789,"sid":"fake-sid","tool":"fake-tool","future":"fake-future"}`+"\n",
			row.module, row.rule, row.verdict)
		rows.WriteString(strings.Repeat(line, row.count))
	}
	return rows.String()
}

// e2eEnv is one isolated world: the built binary, a temp config/home, and a temp git repo as project and cwd.
type e2eEnv struct {
	bin, cfg, home, repo, pathDir string
}

// e2eModuleRoot is the repo root two levels above this file; the build must not run in the temp project.
func e2eModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// e2eRun executes the binary with a from-scratch env (no inherited CLAUDE_*, GIT_* or update-child vars).
func (e e2eEnv) run(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = []string{
		"PATH=" + e.pathDir, // git only: a detached dream/update child cannot find a model CLI
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + filepath.Join(e.home, ".config"),
		"CLAUDE_CONFIG_DIR=" + e.cfg,
		"CLAUDE_PROJECT_DIR=" + e.repo,
		"HARNESS_MEMORY_ORIGIN=" + e.repo, // the repo sits under /tmp: launched-from-workspace, as a harness launch passes
		"TMPDIR=" + e.home,
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", e.bin, args, err)
	}
	return out.String(), errb.String(), code
}

// mustRun fails the test unless the command exits 0.
func (e e2eEnv) mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, errs, code := e.run(t, stdin, args...)
	if code != 0 {
		t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

// hookContext extracts hookSpecificOutput.additionalContext from a hook's stdout.
func hookContext(t *testing.T, stdout string) string {
	t.Helper()
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("hook stdout is not hook JSON: %v\n%q", err, stdout)
	}
	return doc.HookSpecificOutput.AdditionalContext
}

// dirHashes snapshots every regular file under dir (path -> bytes) so a dry run can prove it wrote nothing.
func dirHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			snap[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

var e2eBuild struct {
	once sync.Once
	bin  string
	err  error
}

// e2eCachedBinary asks go run for its cached link; the first run after a change prints a temp path go then deletes.
func e2eCachedBinary(root string) (string, error) {
	var stderr bytes.Buffer
	for range 2 {
		stderr.Reset()
		run := exec.Command("go", "run", "-exec", "echo", "./cmd/claude-code")
		run.Dir, run.Stderr = root, &stderr
		out, err := run.Output()
		if err != nil {
			return "", fmt.Errorf("go run: %v\n%s", err, stderr.String())
		}
		bin := strings.TrimSpace(string(out))
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	return "", fmt.Errorf("go run left no cached claude-code binary\n%s", stderr.String())
}

// e2eBinary resolves the claude-code binary once per test process.
func e2eBinary(t *testing.T) string {
	t.Helper()
	root := e2eModuleRoot(t)
	e2eBuild.once.Do(func() {
		e2eBuild.bin, e2eBuild.err = e2eCachedBinary(root)
	})
	if e2eBuild.err != nil {
		t.Fatal(e2eBuild.err)
	}
	return e2eBuild.bin
}

// e2eRun is the shared state of the lifecycle walkthrough; each method is one subtest, run in order.
type e2eRun struct {
	e       e2eEnv
	cfg     string
	repo    string
	bankDir string
	factID  string
}

// file is the repo bank's top-level file name in the flat store.
func (x *e2eRun) file(name string) string { return Path(name) }

const e2eFact = "rotate the zebrafish kestrel gearbox calibration before every deploy"

// TestE2EMemoryLifecycle drives init, session-start, remember, recall, promote and index through the real binary.
func TestE2EMemoryLifecycle(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	if testing.Short() {
		t.Skip("builds the claude-code binary")
	}
	bin := e2eBinary(t)            // built before isolatedConfig: keeps the real GOCACHE
	cfg, home := isolatedConfig(t) // parent env only locates the on-disk seed; subprocesses get their own env
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	pathDir := canonTemp(t, t.TempDir())
	if err := os.Symlink(gitPath, filepath.Join(pathDir, "git")); err != nil {
		t.Fatal(err)
	}
	repo := gitRepo(t, canonTemp(t, t.TempDir()), "proj")
	r := &e2eRun{e: e2eEnv{bin: bin, cfg: cfg, home: home, repo: repo, pathDir: pathDir}, cfg: cfg, repo: repo}

	initStore(t)                                                      // completed journal + store git repo, written under cfg by the same helper the unit tests use
	write(t, dreamStampPath(), time.Now().UTC().Format(time.RFC3339)) // dream not due: session-start spawns nothing

	for _, step := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"init", r.init}, {"session-start", r.sessionStart}, {"remember", r.remember},
		{"recall-and-search", r.recallAndSearch}, {"recall-hook", r.recallHook},
		{"review-candidates", r.reviewCandidates},
		{"update-procedure", r.updateProcedure}, {"update-transcript", r.updateTranscript},
		{"index-and-fmt", r.indexAndFmt}, {"layout", r.layout},
	} {
		t.Run(step.name, step.run)
	}
}

func (x *e2eRun) init(t *testing.T) {
	out := x.e.mustRun(t, "", "memory", "init")
	if !strings.Contains(out, "bank:") {
		t.Errorf("init output names no bank:\n%s", out)
	}
	x.bankDir = strings.TrimSpace(x.e.mustRun(t, "", "memory", "path"))
	if rel, err := filepath.Rel(StoreDir(), x.bankDir); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("bank dir %s is not under the store %s", x.bankDir, StoreDir())
	}
	for _, name := range BankFiles() {
		if _, err := os.Stat(x.file(name)); err != nil {
			t.Errorf("bank file %s missing: %v", name, err)
		}
	}
}

func (x *e2eRun) sessionStart(t *testing.T) {
	x.e.mustRun(t, "", "memory", "index") // a fresh scaffold has no focus text; the generated catalog is what names the bank
	in := `{"session_id":"e2e-session-start","cwd":"` + x.repo + `","hook_event_name":"SessionStart"}`
	out := x.e.mustRun(t, in, "hook", "SessionStart", "--memory-start")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "Session memory bank") || !strings.Contains(ctx, StoreDir()) {
		t.Errorf("additionalContext does not name the store %s:\n%s", StoreDir(), ctx)
	}
	link := filepath.Join(x.cfg, "projects", paths.ProjectSlug(x.repo), "memory")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("native link %s: %v (mode %v)", link, err, fi)
	}
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(x.bankDir)
	if got != want {
		t.Errorf("native link resolves to %s, want the store bank %s", got, want)
	}
}

func (x *e2eRun) remember(t *testing.T) {
	out := x.e.mustRun(t, "", "memory", "remember", "--type", "feedback", e2eFact)
	m := regexp.MustCompile(`remembered \[([^\]]+)\]`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("remember printed no id:\n%s", out)
	}
	x.factID = m[1]
	// the task text said --type convention; the verb accepts only user|feedback|project|reference
	if _, errs, code := x.e.run(t, "", "memory", "remember", "--type", "convention", "x y z"); code == 0 {
		t.Errorf("--type convention accepted; the flag help lists only user/feedback/project/reference (stderr %q)", errs)
	}
	data, err := os.ReadFile(x.file("conventions.md"))
	if err != nil || !strings.Contains(string(data), "zebrafish kestrel") {
		t.Errorf("fact not in conventions.md (err %v)", err)
	}
}

func (x *e2eRun) recallAndSearch(t *testing.T) {
	if out := x.e.mustRun(t, "", "memory", "recall", "zebrafish", "kestrel"); !strings.Contains(out, x.factID) {
		t.Errorf("recall misses id %s:\n%s", x.factID, out)
	}
	if out := x.e.mustRun(t, "", "memory", "search", "zebrafish", "kestrel"); !strings.Contains(out, x.factID) {
		t.Errorf("search misses id %s:\n%s", x.factID, out)
	}
}

func (x *e2eRun) recallHook(t *testing.T) {
	prompt := "how do I rotate the zebrafish kestrel gearbox calibration before deploy"
	b, _ := json.Marshal(map[string]string{"session_id": "e2e-recall", "cwd": x.repo, "prompt": prompt,
		"hook_event_name": "UserPromptSubmit"})
	out := x.e.mustRun(t, string(b), "hook", "UserPromptSubmit", "--memory-recall")
	ctx := hookContext(t, out)
	if !strings.Contains(ctx, "<!-- memory-recall;") || !strings.Contains(ctx, "zebrafish kestrel") {
		t.Errorf("recall hook injected no matching block:\n%q", out)
	}
}

func (x *e2eRun) reviewCandidates(t *testing.T) {
	x.e.mustRun(t, "", "memory", "review", "--candidates")
}

func (x *e2eRun) updateProcedure(t *testing.T) {
	if out := x.e.mustRun(t, "", "memory", "update"); strings.TrimSpace(out) == "" {
		t.Error("memory update printed no procedure")
	}
}

func (x *e2eRun) updateTranscript(t *testing.T) {
	// copilot and the other agent dispatches are absent from <cfg>/bin, so the chain lands on the fake claude there
	const marker = "e2e-update-marker-7731"
	const session = "e2e-update-session"
	transcript, argvLog, stdinLog := x.fakeTranscriptUpdate(t, marker)
	before := dirHashes(t, x.cfg)

	out := x.e.mustRun(t, "", "memory", "update", "--transcript", transcript, "--cwd", x.repo, "--session", session)
	assertUpdateInvocation(t, transcript, argvLog, stdinLog, out)
	x.assertTranscriptUpdateResult(t, marker, session)
	assertOnlyStoreAndStateGrew(t, x.cfg, before)
}

func (x *e2eRun) fakeTranscriptUpdate(t *testing.T, marker string) (string, string, string) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	argvLog, stdinLog := filepath.Join(t.TempDir(), "argv.log"), filepath.Join(t.TempDir(), "stdin.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argvLog + "'; done\n" +
		"while IFS= read -r l || [ -n \"$l\" ]; do printf '%s\\n' \"$l\"; done > '" + stdinLog + "'\n" + // PATH has no cat
		"printf '\\n- " + marker + "\\n' >> '" + x.file("sessionHistory.md") + "'\n" +
		"echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"updated\"}]}}'\n" +
		"echo '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"updated\"}'\n"
	write(t, filepath.Join(x.cfg, "bin", "claude"), script)
	if err := os.Chmod(filepath.Join(x.cfg, "bin", "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	write(t, transcript, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"+
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"DECISION: ship the zebrafish gearbox"}]}}`+"\n")
	return transcript, argvLog, stdinLog
}

func assertUpdateInvocation(t *testing.T, transcript, argvLog, stdinLog, out string) {
	t.Helper()
	if !strings.Contains(out, "memory update: ran on") {
		t.Errorf("update reported no runner:\n%s", out)
	}
	prompt, _ := os.ReadFile(stdinLog)
	for _, want := range []string{transcript, "Background update", "DECISION: ship the zebrafish gearbox"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("fake claude prompt lacks %q:\n%s", want, prompt)
		}
	}
	if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "--print") {
		t.Errorf("fake claude was not run headless: %q", argv)
	}
}

func (x *e2eRun) assertTranscriptUpdateResult(t *testing.T, marker, session string) {
	t.Helper()
	if hist, _ := os.ReadFile(x.file("sessionHistory.md")); !strings.Contains(string(hist), marker) {
		t.Error("the fake reply's sessionHistory write is missing from the bank")
	}
	res, ok := readUpdateResult(session)
	if !ok || res.Status != updateOK || !res.BankUpdated {
		t.Errorf("update result = %+v ok=%v, want status ok with bank_updated", res, ok)
	}
}

// assertOnlyStoreAndStateGrew checks every path new under cfg since before lives in the store or state/.
func assertOnlyStoreAndStateGrew(t *testing.T, cfg string, before map[string]string) {
	t.Helper()
	store, _ := filepath.EvalSymlinks(filepath.Join(cfg, "memory"))
	for p := range dirHashes(t, cfg) {
		if _, existed := before[p]; existed {
			continue
		}
		if r, _ := filepath.EvalSymlinks(p); !strings.HasPrefix(r, store+string(filepath.Separator)) &&
			!strings.HasPrefix(p, filepath.Join(cfg, "state")+string(filepath.Separator)) {
			t.Errorf("update created %s outside the store and state/", p)
		}
	}
}

func (x *e2eRun) indexAndFmt(t *testing.T) {
	x.e.mustRun(t, "", "memory", "index")
	x.e.mustRun(t, "", "memory", "fmt", "--check")
}

func (x *e2eRun) layout(t *testing.T) {
	if _, err := os.Lstat(filepath.Join(x.cfg, "docs", "memory")); err == nil {
		t.Errorf("legacy tree %s was created beside the live store", filepath.Join(x.cfg, "docs", "memory"))
	}
	store, _ := filepath.EvalSymlinks(filepath.Join(x.cfg, "memory"))
	bankNames := map[string]bool{}
	for _, n := range BankFiles() {
		bankNames[n] = true
	}
	err := filepath.WalkDir(x.cfg, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !bankNames[d.Name()] {
			return nil
		}
		if r, _ := filepath.EvalSymlinks(p); !strings.HasPrefix(r, store+string(filepath.Separator)) {
			t.Errorf("bank file %s lives outside %s", p, store)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func equalSnap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// decisionLogOver writes n newest-first decisionLog.md entries, each with enough distinct tokens to stay distinct.
func decisionLogOver(t *testing.T, path string, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Decision Log\n")
	for i := n; i >= 1; i-- {
		fmt.Fprintf(&b, "\n## decision %02d\n\nrationale topic%d choice%d\n", i, i, i)
	}
	write(t, path, b.String())
}

func TestArchiveOverflowKeepsOldestInArchive(t *testing.T) {
	tempConfig(t)
	decisionLogOver(t, Path("decisionLog.md"), 80)

	n, err := archiveOverflow(time.Now())
	if err != nil || n == 0 {
		t.Fatalf("archived %d err %v", n, err)
	}
	kept, _ := os.ReadFile(Path("decisionLog.md"))
	if n := strings.Count(string(kept), "\n"); n > fileCaps["decisionLog.md"] {
		t.Errorf("local decisionLog still over cap: %d lines", n)
	}
	arch, err := os.ReadFile(archivePath("decisionLog.md", time.Now()))
	if err != nil || !strings.Contains(string(arch), "decision 01") {
		t.Errorf("oldest entry did not land in the archive: %v %q", err, arch)
	}
	if n2, err := archiveOverflow(time.Now()); err != nil || n2 != 0 {
		t.Errorf("second archive: n=%d err=%v, want a no-op", n2, err)
	}
}

func TestDoctorFlagsOversizedAndStale(t *testing.T) {
	tempConfig(t)
	decisionLogOver(t, filepath.Join(StoreDir(), "decisionLog.md"), 80)
	old := time.Now().AddDate(0, 0, -(DefaultStaleDays + 1))
	if err := os.Chtimes(filepath.Join(StoreDir(), "decisionLog.md"), old, old); err != nil {
		t.Fatal(err)
	}
	problems := doctor(StoreDir(), DefaultStaleDays, time.Now())
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "decisionLog.md: over") {
		t.Errorf("want an over-cap finding, got %v", problems)
	}
	if !strings.Contains(joined, "stale") {
		t.Errorf("want a staleness finding, got %v", problems)
	}
}

func TestDoctorFlagsCredentialShapedValue(t *testing.T) {
	tempConfig(t)
	write(t, filepath.Join(StoreDir(), "conventions.md"), "# C\n\n- token AK"+"IAABCDEFGHIJKLMNOP in here\n")
	problems := doctor(StoreDir(), DefaultStaleDays, time.Now())
	if !strings.Contains(strings.Join(problems, "\n"), "possible credential") {
		t.Errorf("want a credential finding, got %v", problems)
	}
}

func TestWriteDoctorNoticeCachesFirstProblem(t *testing.T) {
	tempConfig(t)
	decisionLogOver(t, filepath.Join(StoreDir(), "decisionLog.md"), 80)
	if err := writeDoctorNotice(StoreDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	notice := mustRead(t, doctorNoticePath())
	if !strings.Contains(notice, "decisionLog.md: over") {
		t.Errorf("notice %q", notice)
	}
	// A clean bank caches an EMPTY notice — distinct from no file at all.
	write(t, filepath.Join(StoreDir(), "decisionLog.md"), "# D\n")
	if err := writeDoctorNotice(StoreDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	if notice := mustRead(t, doctorNoticePath()); notice != "" {
		t.Errorf("want an empty cached notice, got %q", notice)
	}
}

func TestLogAccessRedactsCredentialShapedReason(t *testing.T) {
	dir := t.TempDir()
	logAccess(dir, time.Now(), "aa11bb22", "leaked sk-abcdefghijklmnopqrstuvwx in reason")
	data := mustRead(t, filepath.Join(dir, accessLogFile))
	if strings.Contains(data, "sk-abcdefghijklmnopqrstuvwx") {
		t.Error("a credential-shaped reason must be redacted, not logged verbatim")
	}
	if !strings.Contains(data, "redacted") {
		t.Errorf("log %q", data)
	}
}

func TestCollectBankStatsAndTimeline(t *testing.T) {
	tempConfig(t)
	_ = writeUpdateResult("s1", UpdateResult{Status: updateOK, Time: time.Now(), BankUpdated: true})
	_ = writeUpdateResult("s2", UpdateResult{Status: updateFailed, Time: time.Now()})
	write(t, Path("sessionHistory.md"), "# H\n\n## 2026-01-02 — did work\n")

	s := collectBankStats()
	if s.Updates != 2 || s.OK != 1 || s.Failed != 1 || s.BankUpdated != 1 {
		t.Errorf("stats %+v", s)
	}
	report := s.Report()
	if !strings.Contains(report, "success rate: 1/2") {
		t.Errorf("report %q", report)
	}

	entries, sessions, err := timelineBank(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("timeline entries %+v", entries)
	}
	if len(sessions) != 1 || !strings.Contains(sessions[0], "did work") {
		t.Errorf("timeline sessions %+v", sessions)
	}
}

// A journal past the grace window is named with its age; a fresh one (in-flight commit) and a missing dir stay quiet.
func TestJournalProblemsNamesStaleJournal(t *testing.T) {
	isolatedConfig(t)
	now := time.Now()
	if got := journalProblems(now); len(got) != 0 {
		t.Fatalf("missing journal dir: got %q, want none", got)
	}
	dir := JournalDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(dir, "20261109T000000-old.json"), filepath.Join(dir, "20261109T010000-new.json")
	for _, f := range []string{stale, fresh} {
		write(t, f, `{"schema":99}`)
	}
	if err := os.Chtimes(stale, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := journalProblems(now)
	if len(got) != 1 || !strings.Contains(got[0], stale) || !strings.Contains(got[0], "2h0m0s") {
		t.Fatalf("got %q, want one line naming %s and its 2h0m0s age", got, stale)
	}
}

func TestDoctorCachesUnreadableBankWarning(t *testing.T) {
	isolatedConfig(t)
	root := StoreDir()
	if err := os.MkdirAll(filepath.Join(root, "conventions.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	problems := strings.Join(doctor(root, DefaultStaleDays, now), "\n")
	if !strings.Contains(problems, "conventions.md:") || !strings.Contains(problems, "unreadable") {
		t.Fatalf("doctor silently skipped an unreadable bank file: %q", problems)
	}
	if err := writeDoctorNotice(root, DefaultStaleDays, now); err != nil {
		t.Fatal(err)
	}
	notice := mustRead(t, doctorNoticePath())
	if !strings.Contains(notice, "conventions.md:") || !strings.Contains(notice, "unreadable") {
		t.Fatalf("cached healthy notice for an unreadable bank: %q", notice)
	}
}

// TestDoctorReadsEachBankFileOnce: one read per typed file feeds the cap, staleness and credential findings.
func TestDoctorReadsEachBankFileOnce(t *testing.T) {
	isolatedConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, Path("progress.md"), "# Progress\n\n- token sk-ABCDEFGHIJKLMNOPQRSTUV\n")
	reads := countBankReads(t)
	got := doctor(root, DefaultStaleDays, time.Now())
	want := "home progress.md: possible credential-shaped value — scrub before promoting"
	if !slices.Contains(got, want) {
		t.Errorf("doctor = %q, want it to hold %q", got, want)
	}
	if len(reads) < len(bankFiles) {
		t.Fatalf("doctor read %d typed files, want %d: %v", len(reads), len(bankFiles), reads)
	}
	for p, n := range reads {
		if n != 1 {
			t.Errorf("doctor read %s %d times, want once", p, n)
		}
	}
}

// doctorNotice truncates by rune, never splitting a multi-byte character at the 200 boundary.
func TestDoctorNoticeTruncationPreservesUTF8(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	notice := strings.Repeat("A", 196) + "中" + "国" // 202 bytes: a byte cut at 200 splits "国"
	write(t, doctorNoticePath(), notice)
	got := doctorNotice()
	if !strings.HasPrefix(got, " memory review: ") {
		t.Fatalf("doctorNotice prefix not found: %q", got)
	}
	truncated := strings.TrimPrefix(strings.TrimSuffix(got, "."), " memory review: ")
	if !utf8.ValidString(truncated) {
		t.Errorf("truncated notice is invalid UTF-8: %q (bytes: %v)", truncated, []byte(truncated))
	}
	if len(truncated) > 300 {
		t.Errorf("truncation did not work: result is %d bytes", len(truncated))
	}
}

func TestFailedRefreshLeadsReviewAndTheNextSessionStartLine(t *testing.T) {
	runBank(t, func(t *testing.T) {
		decisionLogOver(t, Path("decisionLog.md"), 80) // a competing bank problem the notice must not show first
		if err := os.Mkdir(NativeIndex(), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := settleBankLocked(indexNow); !errors.Is(err, errIndexProblem) || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("err = %v; a MEMORY.md that is not a regular file must refuse the refresh", err)
		}
		if doctorNotice() != " memory review: "+indexNotice+"." {
			t.Errorf("session-start line = %q; the failed settle alone must set the fixed index notice", doctorNotice())
		}
		problems := doctorBank("", DefaultStaleDays, time.Now())
		if len(problems) < 2 || !strings.HasPrefix(problems[0], errIndexProblem.Error()) || !strings.Contains(problems[0], "`claude-code memory index`") {
			t.Fatalf("problems = %q, want the refresh failure with its path-free advice first", problems)
		}
	})
}

// Dream lists feedback notes, its seam writes the candidates file; review accepts it and rejects each bad variant.
func TestCandidatesEndToEnd(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	for _, n := range []string{"feedback/feedback-a.md", "feedback/feedback-b.md", "notes/note-c.md"} {
		write(t, filepath.Join(StoreDir(), filepath.FromSlash(n)), "restated twice\n")
	}
	candidates := filepath.Join(StoreDir(), filepath.FromSlash(candidatesRel))
	good := `{"id":"fc-a","evidence":["feedback/feedback-a.md","notes/note-c.md"],"change":"Add a hook.","target":"unknown","kind":"hook","size":"small"}` + "\n" +
		`{"id":"fc-b","evidence":["feedback/feedback-b.md"],"repeats":3,"change":"Add a verb flag.","target":"rules/standards/x.md","kind":"verb","size":"medium"}`
	file := func(rows string) string { return "# Candidates\n\n```jsonl\n" + rows + "\n```\n" }
	run := func(_ context.Context, prompt, _ string) (string, error) {
		if !strings.Contains(prompt, filepath.Join("feedback", "feedback-a.md")) {
			t.Errorf("dream prompt does not list the changed feedback note: %s", prompt)
		}
		write(t, candidates, file(good))
		return "stub", nil
	}
	if err := dream(dreamOptions{CWD: root, Run: run}); err != nil {
		t.Fatal(err)
	}
	if got := candidateProblems(); len(got) != 0 {
		t.Fatalf("a valid file has problems: %v", got)
	}
	var out strings.Builder
	if err := ReviewReport(&out, ReviewOptions{StaleDays: DefaultStaleDays}); err != nil {
		t.Errorf("review on a valid file failed: %v", err)
	}
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	for _, tc := range candidateVariants(good, secret) {
		write(t, candidates, file(tc.body))
		got := strings.Join(candidateProblems(), "\n")
		if !strings.Contains(got, candidatesRel+tc.want) {
			t.Errorf("%s: problems = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, secret) {
			t.Errorf("%s: a problem row echoes the secret", tc.name)
		}
		if err := ReviewReport(&out, ReviewOptions{StaleDays: DefaultStaleDays}); err == nil {
			t.Errorf("%s: review exited zero", tc.name)
		}
	}
}

type candidateVariant struct{ name, body, want string }

// candidateVariants are malformed copies of good with the problem each must produce.
func candidateVariants(good, secret string) []candidateVariant {
	row := func(old, new string) string { return strings.Replace(good, old, new, 1) }
	return []candidateVariant{
		{"unknown field", row(`"size":"small"`, `"size":"small","extra":1`), ":4: invalid record: unknown field"},
		{"missing evidence", row(`"evidence":["feedback/feedback-a.md","notes/note-c.md"],`, ``), ":4: evidence is missing"},
		{"one file no repeats", row(`,"notes/note-c.md"`, ``), ":4: evidence needs two distinct files"},
		{"missing file", row("note-c.md", "gone.md"), ":4: evidence[1] does not exist"},
		{"target outside", row(`"unknown"`, `"/etc/passwd"`), ":4: target is outside"},
		{"target dotdot", row(`"unknown"`, `"../../etc/passwd"`), ":4: target must use only"},
		{"duplicate id", row(`"fc-b"`, `"fc-a"`), ":5: duplicate id (first at line 4)"},
		{"credential", row("Add a hook.", secret), ":4: credential-shaped"},
		{"bad kind", row(`"hook"`, `"magic"`), ":4: kind must be one of"},
		{"two sentences", row("Add a hook.", "Add a hook. Then more."), ":4: change must be one sentence"},
		{"trailing brace", row(`"size":"small"}`, `"size":"small"}}`), ":4: invalid record: more than one JSON value"},
		{"duplicate key", row(`"kind":"hook"`, `"kind":"hook","kind":"verb"`), ":4: invalid record: duplicate key"},
		{"invalid utf8", row("Add a hook.", "Add \xff hook."), ":4: invalid record: not valid UTF-8"},
	}
}

// Every diagnostic names the line and the reason, never the offending value, wherever the value sits in the record.
func TestCandidateDiagnosticsNeverEchoInput(t *testing.T) {
	isolatedConfig(t)
	const base = `{"id":"fc-a","evidence":["feedback/feedback-a.md","notes/note-c.md"],"change":"Add a hook.","target":"unknown","kind":"hook","size":"small"}`
	rec := func(old, new string) string { return strings.Replace(base, old, new, 1) }
	templates := map[string]string{
		"unknown field": rec(`"size"`, `"@":1,"size"`), "target": rec(`"unknown"`, `"@"`),
		"evidence name": rec(`feedback/feedback-a.md`, `feedback/@.md`), "evidence dir": rec(`feedback/feedback-a.md`, `@/x.md`),
		"id": rec(`fc-a`, `fc-@`), "kind": rec(`"hook"`, `"@"`), "size": rec(`"small"`, `"@"`),
		"repeats type": rec(`"change"`, `"repeats":"@","change"`), "change": rec(`Add a hook.`, `@ one. @ two`),
		"syntax": rec(`"fc-a"`, `@`), "trailing": base + ` @`, "duplicate key": rec(`"size"`, `"@":1,"@":2,"size"`),
		"bad utf8": rec(`Add a hook.`, "\xff@"), "scalar": `"@"`, "array": `["@"]`,
	}
	for _, val := range []string{"zq9fakevalue", "AKIA" + "ABCDEFGHIJKLMNOP"} {
		for name, tpl := range templates {
			text := "```jsonl\n" + strings.ReplaceAll(tpl, "@", val) + "\n" + strings.ReplaceAll(tpl, "@", val) + "\n```\n"
			got := validateCandidates(text)
			if len(got) == 0 {
				t.Errorf("%s: accepted", name)
			}
			if strings.Contains(strings.Join(got, "\n"), val) {
				t.Errorf("%s: a diagnostic echoes %q: %q", name, val, got)
			}
		}
	}
}

// Target and evidence are resolved before the containment test; a symlink out of the store or an alias is refused.
func TestCandidatePathsResolveSymlinks(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	outside := canonTemp(t, t.TempDir())
	write(t, filepath.Join(outside, "n.md"), "x\n")
	write(t, filepath.Join(StoreDir(), "feedback", "feedback-a.md"), "x\n")
	write(t, filepath.Join(StoreDir(), "notes", "note-c.md"), "x\n")
	for _, l := range [][2]string{{outside, filepath.Join(cfg, "linkout")}, {outside, filepath.Join(StoreDir(), "knowledge-out")},
		{filepath.Join(StoreDir(), "feedback", "feedback-a.md"), filepath.Join(StoreDir(), "feedback", "alias.md")},
		{filepath.Join(StoreDir(), "feedback"), filepath.Join(StoreDir(), "notes-alias")}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
	}
	for _, target := range []string{"linkout/x.md", "linkout/n.md", "memory/knowledge-out/new.md", "rules/new.md"} {
		got := targetProblem(target)
		if (target == "rules/new.md") != (got == "") {
			t.Errorf("target %s: problem %q", target, got)
		}
	}
	ev := func(e ...string) string {
		return strings.Join(evidenceProblems(candidate{Evidence: e, Repeats: 0}), "|")
	}
	if got := ev("feedback/feedback-a.md", "feedback/alias.md"); got == "" {
		t.Error("a symlinked evidence file was accepted as a second distinct file")
	}
	if got := ev("feedback/feedback-a.md", "notes/note-c.md"); got != "" {
		t.Errorf("two real files refused: %s", got)
	}
	if got := ev("notes-alias/feedback-a.md"); got == "" {
		t.Error("evidence outside feedback/ and notes/ accepted")
	}
	if upper := filepath.Join(StoreDir(), "feedback", "FEEDBACK-A.md"); paths.FoldsCase {
		if _, err := os.Stat(upper); err == nil && ev("feedback/feedback-a.md", "feedback/FEEDBACK-A.md") == "" {
			t.Error("two spellings of one file counted as two")
		}
	}
}

// The change cap counts characters, not bytes.
func TestOneSentenceCountsRunes(t *testing.T) {
	if !oneSentence(strings.Repeat("é", candidateMaxText)) || oneSentence(strings.Repeat("é", candidateMaxText+1)) {
		t.Error("the cap is not 200 runes")
	}
}
