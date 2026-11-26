package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// A failed run's retry deadline is independent of changes to either bank.
func TestPersistenceFailedDreamRetriesWithOldBanks(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("agent unavailable")
	run := func(context.Context, string, string) (string, error) { return "stub", failure }
	if err := dream(dreamOptions{CWD: root, Run: run}); !errors.Is(err, failure) {
		t.Fatalf("Dream = %v, want agent failure", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for name := range templates() {
		path := Path(name)
		write(t, path, templates()[name])
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := newestMtime(); got.After(old) {
		t.Fatalf("bank fixture has recent mtime: %v", got)
	}
	if dreamDue(now.Add(dreamRetry - time.Minute)) {
		t.Error("failed dream is due before six-hour backoff")
	}
	if !dreamDue(now.Add(dreamRetry + time.Minute)) {
		t.Error("failed dream with two-day-old banks is not due after six hours")
	}
}

// TestPreviewDreamReadsEachBankFileOnce: the cap, archive and rotation previews share one read per typed file.
func TestPreviewDreamReadsEachBankFileOnce(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	var conv, hist strings.Builder
	conv.WriteString("# Conventions\n")
	for i := range 90 {
		fmt.Fprintf(&conv, "\n- Feedback: rule %d", i)
	}
	hist.WriteString("# Session History\n")
	for i := range sessionHistoryMaxEntries + 5 {
		fmt.Fprintf(&hist, "\n## 2026-10-01 — session %d\n\nbody\n", i)
	}
	write(t, Path("conventions.md"), conv.String()+"\n")
	write(t, Path("sessionHistory.md"), hist.String())
	_, _, overflow := overflowSplit("conventions.md", conv.String()+"\n", archiveCaps["conventions.md"])
	if len(overflow) == 0 {
		t.Fatal("fixture has no conventions overflow")
	}
	reads := countBankReads(t)
	p := previewDream()
	if p.Archivable != len(overflow) || p.SessionRotate != 5 || !slices.Equal(p.Oversized, []string{"conventions.md"}) {
		t.Errorf("previewDream = %+v, want Archivable %d, SessionRotate 5, Oversized [conventions.md]", p, len(overflow))
	}
	for path, n := range reads {
		if n != 1 {
			t.Errorf("previewDream read %s %d times, want once", path, n)
		}
	}
}

// The dry-run report pluralizes sessionHistory rotations the way the package's other counts do.
func TestDreamReportPluralizesRotatedEntries(t *testing.T) {
	got := dreamPreview{Dir: "d", SessionRotate: 3}.Report()
	if !strings.Contains(got, "would rotate 3 sessionHistory entr(ies) to archive/\n") {
		t.Fatalf("Report() = %q, want the entr(ies) rotation line", got)
	}
}

// TestPreviewDreamReportsUnreadableBankFile: a bank file that cannot be read is named, never "nothing to consolidate".
func TestPreviewDreamReportsUnreadableBankFile(t *testing.T) {
	tempConfig(t)
	locked := Path("decisionLog.md")
	orig := readFile
	readFile = func(p string) ([]byte, error) {
		if p == locked {
			return nil, fs.ErrPermission
		}
		return orig(p)
	}
	t.Cleanup(func() { readFile = orig })
	got := previewDream().Report()
	if !strings.Contains(got, "home decisionLog.md: permission denied — unreadable") || strings.Contains(got, "nothing to consolidate") {
		t.Errorf("dry run hid the unreadable bank file:\n%s", got)
	}
}

// Given a peer dreamed after this worker's due check, When it takes the lock, Then it skips and leaves the stamp.
func TestDreamIfDueSkipsUnderLockWhenPeerJustDreamed(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	peer := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := stampDream(peer, 0); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) {
		t.Error("a second dream ran right after a peer's")
		return "stub", nil
	}
	var out strings.Builder
	if err := dream(dreamOptions{CWD: root, Run: run, Out: &out, IfDue: true}); err != nil {
		t.Fatalf("dream = %v, want nil", err)
	}
	if got, _, _ := dreamStamp(); !got.Equal(peer) {
		t.Errorf("stamp = %v, want the peer's %v untouched", got, peer)
	}
	if !strings.Contains(out.String(), "not due") {
		t.Errorf("output %q lacks the not-due note", out.String())
	}
}

// Flat banks share one dir: every changed store file is listed once, none dropped, none repeated per bank.
func TestDreamChangedFlatListsEachFileOnce(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), "knowledge--topic.md"), "x\n")
	got := dreamChanged(time.Time{})
	for _, want := range []string{"progress.md", "knowledge--topic.md"} {
		if n := countBase(got, want); n != 1 {
			t.Errorf("dreamChanged lists %s %d times, want 1: %v", want, n, got)
		}
	}
}

func countBase(paths []string, base string) int {
	n := 0
	for _, p := range paths {
		if filepath.Base(p) == base {
			n++
		}
	}
	return n
}

func TestDreamRetryBackoff(t *testing.T) {
	for fails, want := range map[int]time.Duration{1: 6 * time.Hour, 2: 12 * time.Hour, 3: 24 * time.Hour, 9: 24 * time.Hour} {
		if got := retryAfter(fails); got != want {
			t.Errorf("retryAfter(%d) = %v, want %v", fails, got, want)
		}
	}
	isolatedConfig(t)
	for n := 1; n <= 3; n++ {
		if err := stampDream(time.Now(), n); err != nil {
			t.Fatal(err)
		}
		if _, got, _ := dreamStamp(); got != n {
			t.Fatalf("dreamStamp fails = %d, want %d", got, n)
		}
	}
	if err := stampDream(time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if _, fails, _ := dreamStamp(); fails != 0 {
		t.Fatalf("success must reset the count, got %d", fails)
	}
}

func TestDreamRunLineIsTruthfulForAFailedRun(t *testing.T) {
	if got := dreamRunLine("claude", nil, false); got != "memory dream: ran on claude" {
		t.Errorf("ok line = %q", got)
	}
	got := dreamRunLine("claude", errors.New("Not logged in\nexit 3"), false)
	if strings.Contains(got, "ran on") || got != "memory dream: failed on claude: Not logged in" {
		t.Errorf("failed line = %q", got)
	}
}

// Every dream run leaves one record: a failed run, then a not-due run, read back newest first.
func TestDreamRecordsEveryRun(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, string, string) (string, error) { return "stub", errors.New("boom\nsecond line") }
	idle := func(ctx context.Context, _, _ string) (string, error) {
		AnswerJob(ctx, `{"ops":[]}`)
		return "stub", nil
	}
	runs := []dreamOptions{
		{CWD: root, Run: fail, Trigger: "manual", Forced: true},
		{CWD: root, Run: idle, IfDue: true, Trigger: "schedule"},
	}
	for _, o := range runs {
		_ = dream(o)
	}
	got, err := LastDreamResults(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("LastDreamResults = %v, %v; want 2 records", got, err)
	}
	brief := func(r DreamResult) string {
		return fmt.Sprintf("%s|%s|%s|forced=%t|log=%t", r.Trigger, r.Outcome, r.Reason, r.Forced, r.Log != "")
	}
	if g := brief(got[0]) + " / " + brief(got[1]); g != "schedule|noop|not due|forced=false|log=false / manual|failed|boom|forced=true|log=true" {
		t.Errorf("records = %s", g)
	}
}

func TestDreamLogsAreUniqueAndPruned(t *testing.T) {
	isolatedConfig(t)
	dir := paths.MemoryStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range dreamLogKeep + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("dream-20200101-000000.%03d-old.log", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := openDreamLog()
	if err != nil {
		t.Fatal(err)
	}
	b, err := openDreamLog()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if a.Name() == b.Name() {
		t.Errorf("same log name %s", a.Name())
	}
	if logs, _ := filepath.Glob(filepath.Join(dir, "dream-[0-9]*.log")); len(logs) != dreamLogKeep {
		t.Errorf("%d logs kept, want %d", len(logs), dreamLogKeep)
	}
}

// Output written before dream() (catch-up) and by the runner through JobOutput both land in the per-run log.
func TestDreamLogCapturesCatchupAndRunnerOutput(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, _, _ string) (string, error) {
		AnswerJob(ctx, `{"ops":[]}`)
		fmt.Fprintln(JobOutput(ctx, io.Discard), "hop-receipt-line")
		return "stub", nil
	}
	o := dreamOptions{CWD: root, Run: run, Trigger: "manual", Forced: true}.withLog()
	fmt.Fprintln(o.Out, "catchup-line")
	_ = dream(o)
	got, _ := LastDreamResults(1)
	if len(got) != 1 || got[0].Log == "" {
		t.Fatalf("results = %v, want one with a log", got)
	}
	b, _ := os.ReadFile(got[0].Log)
	if !strings.Contains(string(b), "catchup-line") || !strings.Contains(string(b), "hop-receipt-line") {
		t.Errorf("log = %q, want catch-up and runner lines", b)
	}
}

// A log a retained result record names survives pruning past the keep count.
func TestDreamPruneKeepsNamedLogs(t *testing.T) {
	isolatedConfig(t)
	dir := paths.MemoryStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(dir, "dream-20200101-000000.000-a.log")
	other := filepath.Join(dir, "dream-20200101-000000.001-b.log")
	for _, p := range []string{named, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := appendDreamResult(DreamResult{At: time.Now(), Log: named}); err != nil {
		t.Fatal(err)
	}
	pruneDreamLogs(dir, 0)
	if _, err := os.Stat(named); err != nil {
		t.Errorf("named log pruned: %v", err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("unnamed log kept")
	}
}

// A canned typed answer applies outside the runner's lock scope and records only successful batches.
func TestDreamAppliesTypedBatch(t *testing.T) {
	t.Run("post-commit result", dreamTestPostCommit)
	t.Run("mechanical archive", dreamTestArchive)
	t.Run("snapshot limits and ambiguity", dreamTestSnapshot)
	cases := []struct {
		name, answer string
		changed      []string
		fail, race   bool
	}{
		{"apply", `{"ops":[{"op":"update","id":"UPDATE","read":"UPDATE","text":"Keep the verified next step"},{"op":"close","id":"CLOSE","read":"CLOSE","reason":"done"},{"op":"supersede","id":"1234abcd","read":"RULE","text":"Prefer the verified workflow"},{"op":"noop"}]}`, []string{"conventions.md", "progress.md"}, false, false},
		{"identical update", `{"ops":[{"op":"update","id":"UPDATE","text":"Pending work"}]}`, nil, false, false},
		{"add rejected", `{"ops":[{"op":"add","target":"conventions","text":"Unrequested addition"}]}`, nil, true, false},
		{"unknown id", `{"ops":[{"op":"close","id":"z00000000","reason":"done"}]}`, nil, true, false},
		{"unknown by", `{"ops":[{"op":"close","id":"CLOSE","reason":"superseded-by","by":"deadbeef"}]}`, nil, true, false},
		{"snapshot mismatch", `{"ops":[{"op":"update","id":"1234abcd","read":"z00000000","text":"Wrong read"}]}`, nil, true, false},
		{"stored id race", `{"ops":[{"op":"update","id":"1234abcd","read":"RULE","text":"Wrong write"}]}`, nil, true, true},
		{"noop", `{"ops":[{"op":"noop","note":"Nothing justified"}]}`, nil, false, false},
		{"empty", `{"ops":[]}`, nil, false, false},
		{"invalid", `{"ops":[{"op":"close","id":"CLOSE","reason":"guess"}]}`, nil, true, false},
		{"no answer", ``, nil, true, false},
		{"target rejected", `{"ops":[{"op":"update","target":"progress","id":"UPDATE","text":"Wrong routing"}]}`, nil, true, false},
		{"missing", `{"ops":`, nil, true, false},
		{"unknown field", `{"ops":[],"edits":[]}`, nil, true, false},
		{"trailing object", `{"ops":[]} {}`, nil, true, false},
		{"atomic failure", `{"ops":[{"op":"close","id":"CLOSE","reason":"done"},{"op":"update","id":"UPDATE","text":"bad\n~~~"}]}`, nil, true, false},
		{"changed since read", `{"ops":[{"op":"update","id":"UPDATE","text":"Overwrite concurrent work"}]}`, nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatedConfig(t)
			shortBankWait(t)
			initTypedStore(t)
			progress, conventions := dreamTestFixture(t)
			write(t, Path("progress.md"), progress)
			write(t, Path("conventions.md"), conventions)
			refs := dreamTestRefs(t, progress, conventions)
			answer := strings.NewReplacer("UPDATE", refs[0], "CLOSE", refs[1], "RULE", refs[2]).Replace(tc.answer)
			run := dreamTestRunner(t, answer, refs, tc.race)
			err := dream(dreamOptions{CWD: t.TempDir(), Run: run, Trigger: "manual", Forced: true})
			if (err != nil) != tc.fail {
				t.Fatalf("%s: dream = %v, want failure %t", tc.name, err, tc.fail)
			}
			dreamTestResult(t, tc.changed, tc.fail, tc.name)
			dreamTestContents(t, tc.name, tc.race, progress, conventions)
		})
	}
}

func dreamTestFixture(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	return "# Progress\n\n## In Progress\n\n- Pending work\n- Completed work\n",
		"# Conventions\n\n- Prefer the old workflow <!-- id:1234abcd -->\n"
}

func dreamTestContents(t *testing.T, name string, race bool, progress, conventions string) {
	t.Helper()
	if name == "apply" {
		dreamTestApplied(t)
		return
	}
	if race {
		progress = "# Progress\n\n## In Progress\n\n- Concurrent work\n- Completed work\n"
		conventions = "# Conventions\n\n- Concurrent rule <!-- id:1234abcd -->\n"
	}
	for file, want := range map[string]string{"progress.md": progress, "conventions.md": conventions} {
		if b, _ := os.ReadFile(Path(file)); string(b) != want {
			t.Errorf("%s changed on %s: %s", file, name, b)
		}
	}
}

func dreamTestRefs(t *testing.T, progress, conventions string) []string {
	t.Helper()
	p := ParseDoc("progress.md", []byte(progress))
	c := ParseDoc("conventions.md", []byte(conventions))
	return []string{contentRef(p.Entries[1].block()), contentRef(p.Entries[2].block()), contentRef(c.Entries[0].block())}
}

func dreamTestRunner(t *testing.T, answer string, refs []string, race bool) AgentFunc {
	t.Helper()
	return func(ctx context.Context, prompt, _ string) (string, error) {
		if JobAnswerSchema(ctx) != DreamBatchSchema() {
			t.Error("runner did not receive Batch schema")
		}
		for _, ref := range append(refs, `"id":"1234abcd"`) {
			if !strings.Contains(prompt, ref) {
				t.Errorf("snapshot missing %s", ref)
			}
		}
		// Taking the same store lock in the runner proves the model is not run under it.
		if err := withBankLock(BankLockWait, func() error {
			if race {
				write(t, Path("progress.md"), "# Progress\n\n## In Progress\n\n- Concurrent work\n- Completed work\n")
				write(t, Path("conventions.md"), "# Conventions\n\n- Concurrent rule <!-- id:1234abcd -->\n")
			}
			return nil
		}); err != nil {
			t.Fatalf("store lock held during agent run: %v", err)
		}
		ReportJob(ctx, JobResult{Agent: "stub", Profile: "qa", Model: "canned"})
		AnswerJob(ctx, answer)
		return "stub", nil
	}
}

func dreamTestResult(t *testing.T, changed []string, fail bool, name string) {
	t.Helper()
	records, err := LastDreamResults(2)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %v, %v", records, err)
	}
	r := records[0]
	want := DreamNoop
	if fail {
		want = DreamFailed
	} else if len(changed) > 0 {
		want = DreamOK
	}
	if r.Outcome != want || !slices.Equal(r.Changed, changed) {
		t.Errorf("result = %+v, want %s, changed %v", r, want, changed)
	}
	if want == DreamNoop && r.Reason != "no bank change" {
		t.Errorf("noop reason = %q", r.Reason)
	}
	dreamTestDiagnostics(t, r, name, fail)
}

func dreamTestDiagnostics(t *testing.T, r DreamResult, name string, fail bool) {
	t.Helper()
	log := mustRead(t, r.Log)
	if !strings.Contains(log, "ran on stub") {
		t.Errorf("missing runner receipt: %s", log)
	}
	if !fail && name != "empty" && !strings.Contains(log, "op 0:") {
		t.Errorf("missing op diagnostics: %s", log)
	}
	if strings.Contains(log, "op 0: noop") && !strings.Contains(log, "note: Nothing justified") {
		t.Errorf("noop note not logged: %s", log)
	}
	if r.Agent != "stub" || r.Profile != "qa" || r.Model != "canned" {
		t.Errorf("route report lost: %+v", r)
	}
}

func dreamTestApplied(t *testing.T) {
	t.Helper()
	p, _ := os.ReadFile(Path("progress.md"))
	c, _ := os.ReadFile(Path("conventions.md"))
	if !strings.Contains(string(p), "Keep the verified next step") || strings.Contains(string(p), "Completed work") {
		t.Errorf("progress batch not applied: %s", p)
	}
	for _, want := range []string{"Prefer the verified workflow", "Replaced by", "Supersedes 1234abcd"} {
		if !strings.Contains(string(c), want) {
			t.Errorf("conventions missing %q: %s", want, c)
		}
	}
}

func dreamTestPostCommit(t *testing.T) {
	isolatedConfig(t)
	initTypedStore(t)
	write(t, Path("conventions.md"), "# Conventions\n\n- Old rule <!-- id:1234abcd -->\n")
	_, base, files, docs, err := dreamListing()
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{Ops: []Op{{Op: "update", ID: "1234abcd", Read: contentRefs(docs)["1234abcd"], Text: "New rule"}}}
	duplicate := dreamRan{base: Versions{"1234abcd": ""}, files: map[string]string{"1234abcd": ""}}
	if err := duplicate.checkBatch(batch); err == nil {
		t.Fatal("ambiguous snapshot ref accepted")
	}
	now := time.Now()
	planned := maps.Clone(docs)
	based, err := withBase(batch, contentRefs(docs))
	if err != nil {
		t.Fatal(err)
	}
	runBatchOps(planned, based, now)
	results, err := newBankStore(false).Apply(batch, base, now)
	if err != nil {
		t.Fatal(err)
	}
	r := dreamRan{base: base, files: files, docs: docs}
	current, _ := os.ReadFile(Path("conventions.md"))
	write(t, Path("conventions.md"), string(current)+"\n- Unrelated concurrent rule <!-- id:5678abcd -->\n")
	r.recordApplied(results, planned, errors.New("post-commit settlement failed"))
	if !slices.Equal(r.changed, []string{"conventions.md"}) {
		t.Errorf("committed changes lost after settlement error: %v", r.changed)
	}
	write(t, Path("conventions.md"), string(docs["conventions.md"].Render()))
	r.changed = nil
	r.recordApplied(results, planned, errors.New("commit failed before writing"))
	if len(r.changed) != 0 {
		t.Errorf("uncommitted changes recorded: %v", r.changed)
	}
}

func dreamTestArchive(t *testing.T) {
	isolatedConfig(t)
	initTypedStore(t)
	var rules strings.Builder
	rules.WriteString("# Conventions\n")
	for i := range 90 {
		fmt.Fprintf(&rules, "\n- Invented rule %d <!-- id:%08x -->\n", i, i+1)
	}
	write(t, Path("conventions.md"), rules.String())
	run := func(ctx context.Context, _, _ string) (string, error) {
		AnswerJob(ctx, `{"ops":[]}`)
		return "stub", nil
	}
	if err := dream(dreamOptions{CWD: t.TempDir(), Run: run}); err != nil {
		t.Fatal(err)
	}
	records, err := LastDreamResults(1)
	if err != nil || len(records) != 1 || records[0].Archived == 0 || records[0].Outcome != DreamOK {
		t.Fatalf("mechanical archive did not run: %v, %v", records, err)
	}
	if len(records[0].Changed) != 0 {
		t.Errorf("noop batch invented changed files: %v", records[0].Changed)
	}
}

func dreamTestSnapshot(t *testing.T) {
	isolatedConfig(t)
	initTypedStore(t)
	write(t, Path("progress.md"), "# Progress\n\n## In Progress\n\n- Duplicate\n- Duplicate\n- Unique <path> & value\n")
	listing, base, files, _, err := dreamListing()
	if err != nil {
		t.Fatal(err)
	}
	ref := contentRef("- Duplicate")
	if _, exists := base[ref]; !exists || base[ref] != "" || files[ref] != "" {
		t.Fatalf("duplicate snapshot ref was not marked ambiguous: %v / %v", base, files)
	}
	r := dreamRan{base: base, files: files}
	for _, op := range []Op{
		{Op: "close", ID: ref, Reason: "done"},
		{Op: "close", ID: contentRef("- Unique <path> & value"), Reason: "superseded-by", By: ref},
	} {
		if err := r.checkBatch(Batch{Ops: []Op{op}}); err == nil || !strings.Contains(err.Error(), "ambiguous snapshot") {
			t.Errorf("expected snapshot ambiguity, got %v", err)
		}
	}
	dreamTestListingText(t, listing)
	write(t, Path("progress.md"), "# Progress\n\n- "+strings.Repeat("x", 256*1024)+"\n")
	if _, _, _, _, err := dreamListing(); err == nil {
		t.Error("oversized model snapshot accepted")
	}
}

func dreamTestListingText(t *testing.T, listing string) {
	t.Helper()
	if !strings.Contains(listing, `"writable":false`) || !strings.Contains(listing, `"writable":true`) {
		t.Error("snapshot does not distinguish ambiguous refs")
	}
	if !strings.Contains(listing, "<path> & value") || strings.Contains(listing, `\u003c`) {
		t.Error("snapshot HTML-escapes entry text")
	}
	if !strings.Contains(listing, `"file":"progress.md"`) {
		t.Error("snapshot lacks lowercase field names")
	}
}

// initTypedStore makes a temp store with the current layout stamp.
func initTypedStore(t *testing.T) {
	t.Helper()
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), layoutFile), fmt.Sprintf("flat %d\n", storeVersion))
}

// A store stamped flat 3 still runs the typed path: the stamp is ignored.
func TestDreamIgnoresOlderLayoutStamp(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), layoutFile), "flat 3\n")
	var out strings.Builder
	typed := false
	run := func(ctx context.Context, prompt, _ string) (string, error) {
		typed = JobAnswerSchema(ctx) != "" && strings.Contains(prompt, "Bank entries (JSON snapshot")
		AnswerJob(ctx, `{"ops":[{"op":"noop"}]}`)
		return "stub", nil
	}
	if err := dream(dreamOptions{CWD: t.TempDir(), Run: run, Out: &out, Forced: true}); err != nil {
		t.Fatal(err)
	}
	if !typed {
		t.Error("a store without the current stamp must run the typed path")
	}
}

func TestDreamApplySideProposesCandidatesAndTests(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "feedback", "feedback-a.md"), "x\n")
	write(t, filepath.Join(paths.MemoryStoreDir(), "notes", "note-c.md"), "x\n")
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	good := candidate{ID: "fc-a", Evidence: []string{"feedback/feedback-a.md", "notes/note-c.md"}, Repeats: 1, Change: "Add a hook.", Target: "unknown", Kind: "hook", Size: "small"}
	bad := good
	bad.ID, bad.Kind = "fc-b", "nonsense"
	var out strings.Builder
	var r dreamRan
	err := r.applySide(Batch{Candidates: []candidate{bad, good}, TestProposals: []testProposal{
		{Pkg: "internal/x", Case: "empty input", Reason: "nil path"}, {Pkg: "internal/x", Case: "no reason"}}}, &out)
	if err == nil || !strings.Contains(err.Error(), "not written: [candidate fc-b, test proposal no reason]") {
		t.Errorf("partial side result: %v", err)
	}
	data := mustRead(t, filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel)))
	if !strings.Contains(data, `"fc-a"`) || strings.Contains(data, "fc-b") {
		t.Errorf("candidates file: %s", data)
	}
	tests := mustRead(t, testProposalsFile())
	if !strings.Contains(tests, "empty input") || strings.Contains(tests, "no reason") || !strings.Contains(tests, `"source":"dream"`) {
		t.Errorf("test proposals: %s", tests)
	}
	if !slices.Equal(r.changed, []string{candidatesRel, "techdebt/tests.jsonl"}) {
		t.Errorf("changed = %v, want the candidates and test-proposal files", r.changed)
	}
	for _, want := range []string{"candidate fc-b: rejected", "test proposal no reason: rejected"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("log lacks %q: %s", want, out.String())
		}
	}
}

func TestDreamApplySidePlanTasks(t *testing.T) {
	isolatedConfig(t)
	work := hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	plan := filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-side.md")
	write(t, plan, "# P\n\n- [ ] Phase 1.1 One\n- [ ] Phase 1.2 Two\n")
	plan = plans.OpenTasks(work)[0].Plan // the listed spelling, not the temp dir's symlinked one
	r := dreamRan{plan: startPlanPhase(work)}
	var out strings.Builder
	r.applySide(Batch{PlanTasks: []PlanTaskOp{
		{Plan: plan, ID: "Phase 1.2", Action: "done", Sha: "abc1234"},
		{Plan: plan, ID: "Phase 9.9", Action: "done", Sha: "abc1234"},
		{Plan: plan + ".other", ID: "Phase 1.1", Action: "done", Sha: "abc1234"},
		{Plan: plan, ID: "Phase 1.1", Action: "strike", Reason: "obsolete", Evidence: "trust me"},
		{Plan: plan, ID: "Phase 1.1", Action: "done"},
	}}, &out)
	body := mustRead(t, plan)
	if !strings.Contains(body, "[x] Phase 1.2") || strings.Contains(body, "[x] Phase 1.1") || strings.Contains(body, "~~") {
		t.Errorf("plan after side ops:\n%s", body)
	}
	if n := strings.Count(out.String(), "rejected"); n != 4 {
		t.Errorf("%d rejections, want 4: %s", n, out.String())
	}
	out.Reset()
	r.applySide(Batch{PlanTasks: []PlanTaskOp{{Plan: plan, ID: "Phase 1.2", Action: "done", Sha: "abc1234"}}}, &out)
	if !strings.Contains(out.String(), "already closed") {
		t.Errorf("a closed task was accepted again: %s", out.String())
	}
	r.applySide(Batch{PlanTasks: []PlanTaskOp{{Plan: plan, ID: "Phase 1.1", Action: "strike", Reason: "obsolete", Evidence: "note:feedback/x.md"}}}, &out)
	if !strings.Contains(mustRead(t, plan), "~~") || !slices.Contains(r.changed, "plans/"+filepath.Base(plan)) {
		t.Errorf("strike not applied or not counted (changed %v):\n%s", r.changed, mustRead(t, plan))
	}
}

func TestDreamPlanTaskClosedByPeerIsNotOurs(t *testing.T) {
	isolatedConfig(t)
	work := hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	plan := filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-first.md")
	write(t, plan, "# P\n\n- [ ] Phase 1.1 One\n")
	plan = plans.OpenTasks(work)[0].Plan
	r := dreamRan{plan: startPlanPhase(work)}
	if _, err := plans.MarkDone(plan, "Phase 1.1", "abc1234"); err != nil { // the peer, after the prompt listed it
		t.Fatal(err)
	}
	var out strings.Builder
	r.applySide(Batch{PlanTasks: []PlanTaskOp{{Plan: plan, ID: "Phase 1.1", Action: "done", Sha: "abc1234"}}}, &out)
	if len(r.plan.wrote) != 0 || len(r.changed) != 0 {
		t.Errorf("a peer's closure was recorded as ours: wrote %v changed %v", r.plan.wrote, r.changed)
	}
}

// A peer's edit during the run is never undone; a run's own unproven write is, until a peer touches the plan too.
func TestDreamFinishLeavesPeerPlanEdits(t *testing.T) {
	isolatedConfig(t)
	work := hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	plan := filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-peer.md")
	write(t, plan, "# P\n\n- [ ] Phase 1.1 One\n- [ ] Phase 1.2 Two\n- [ ] Phase 1.3 Three\n- [ ] Phase 1.4 Four\n")
	plan = plans.OpenTasks(work)[0].Plan
	d := startPlanPhase(work)
	r := dreamRan{plan: d}
	var out strings.Builder
	r.applySide(Batch{PlanTasks: []PlanTaskOp{{Plan: plan, ID: "Phase 1.1", Action: "done", Sha: "abc1234"}}}, &out)
	peer := strings.Replace(mustRead(t, plan), "- [ ] Phase 1.2 Two", "- [x] Phase 1.2 Two (abc1234)", 1)
	write(t, plan, peer)
	d.finish(&out)
	body := mustRead(t, plan)
	if body != peer || !strings.Contains(out.String(), "flagged") {
		t.Errorf("peer edit or own write was reverted (want both left, logged as flagged):\n%s\n%s", body, out.String())
	}
	r2 := dreamRan{plan: startPlanPhase(work)}
	r2.applySide(Batch{PlanTasks: []PlanTaskOp{{Plan: plan, ID: "Phase 1.3", Action: "done", Sha: "abc1234"}, {Plan: plan, ID: "Phase 1.4", Action: "done", Sha: "abc1234"}}}, &out)
	r2.dropReverted(r2.plan.finish(&out))
	if slices.Contains(r2.changed, "plans/"+filepath.Base(plan)) {
		t.Errorf("reverted writes still counted as changed: %v", r2.changed)
	}
	if strings.Contains(mustRead(t, plan), "[x] Phase 1.3") || strings.Contains(mustRead(t, plan), "[x] Phase 1.4") {
		t.Errorf("an unproven write of this run stayed:\n%s", mustRead(t, plan))
	}
}

func TestDreamBatchSchemaCarriesSideArrays(t *testing.T) {
	if strings.Contains(BatchSchema(), "plan_tasks") || !strings.Contains(DreamBatchSchema(), "plan_tasks") {
		t.Error("only the dream schema carries the side arrays")
	}
	typed := dreamProcedure()
	for _, phase := range []string{"## Phase 5b", "## Phase 5c", "## Phase 5d"} {
		if strings.Count(typed, phase) != 1 {
			t.Errorf("typed procedure lacks %q exactly once", phase)
		}
	}
}

func TestDreamApplyFailureFallsBackOnce(t *testing.T) {
	stale := `{"ops":[{"op":"close","id":"z00000000","reason":"done"}]}`
	for _, name := range []string{"corrected", "still bad"} {
		t.Run(name, func(t *testing.T) {
			isolatedConfig(t)
			shortBankWait(t)
			initTypedStore(t)
			progress, conventions := dreamTestFixture(t)
			write(t, Path("progress.md"), progress)
			write(t, Path("conventions.md"), conventions)
			refs := dreamTestRefs(t, progress, conventions)
			fixed := stale
			if name == "corrected" {
				fixed = `{"ops":[{"op":"close","id":"` + refs[1] + `","read":"` + refs[1] + `","reason":"done"}]}`
			}
			calls := 0
			standInStoreCopy(t)
			old := CodeModeRun
			CodeModeRun = func(_ context.Context, prompt, _, _, _ string) (string, string, error) {
				calls++
				if !strings.Contains(prompt, "Your previous answer failed") {
					t.Error("fallback prompt lacks the failure")
				}
				return fixed, "fake-pi", nil
			}
			t.Cleanup(func() { CodeModeRun = old })
			err := dream(dreamOptions{CWD: t.TempDir(), Run: dreamTestRunner(t, stale, refs, false), Trigger: "manual", Forced: true})
			got, _ := os.ReadFile(Path("progress.md"))
			changed := string(got) != progress
			if calls != 1 || changed != (name == "corrected") || (err != nil) != (name == "still bad") ||
				err != nil && !strings.Contains(err.Error(), "fake-pi") {
				t.Errorf("dispatches %d, changed %v, err %v", calls, changed, err)
			}
		})
	}
}

func sideRollbackFixture(t *testing.T) (dreamRan, Batch, []string) {
	isolatedConfig(t)
	initTypedStore(t)
	work := hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	plan := filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-side.md")
	write(t, plan, "# P\n\n- [ ] Phase 1.1 One\n")
	plan = plans.OpenTasks(work)[0].Plan
	write(t, filepath.Join(paths.MemoryStoreDir(), "feedback", "feedback-a.md"), "x\n")
	write(t, filepath.Join(paths.MemoryStoreDir(), "notes", "note-c.md"), "x\n")
	candidateFile := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel))
	write(t, candidateFile, "# Feedback candidates\n\n```jsonl\n```\n")
	write(t, testProposalsFile(), "{\"pkg\":\"old\",\"case\":\"old\",\"reason\":\"old\"}\n")
	write(t, Path("conventions.md"), "# Conventions\n\n- Old rule <!-- id:1234abcd -->\n")
	_, base, files, docs, err := dreamListing()
	if err != nil {
		t.Fatal(err)
	}
	b := Batch{Ops: []Op{{Op: "update", ID: "1234abcd", Read: contentRefs(docs)["1234abcd"], Text: "New rule"}},
		Candidates:    []candidate{{ID: "fc-a", Evidence: []string{"feedback/feedback-a.md", "notes/note-c.md"}, Repeats: 1, Change: "Add a hook.", Target: "unknown", Kind: "hook", Size: "small"}},
		TestProposals: []testProposal{{Pkg: "internal/x", Case: "empty input", Reason: "nil path"}},
		PlanTasks:     []PlanTaskOp{{Plan: plan, ID: "Phase 1.1", Action: "done", Sha: "abc1234"}}}
	return dreamRan{base: base, files: files, docs: docs, plan: startPlanPhase(work)}, b, []string{candidateFile, testProposalsFile(), plan}
}

func TestDreamSideEntriesWaitForBank(t *testing.T) {
	r, b, names := sideRollbackFixture(t)
	before := make([]string, len(names))
	for i, name := range names {
		before[i] = mustRead(t, name)
	}
	write(t, Path("conventions.md"), "# Conventions\n\n- Peer rule <!-- id:1234abcd -->\n")
	answer, _ := json.Marshal(b)
	if err := r.applyAnswer(string(answer), io.Discard); err == nil || !refused(err) {
		t.Fatalf("expected bank refusal: %v", err)
	}
	for i, name := range names {
		if got := mustRead(t, name); got != before[i] {
			t.Errorf("side file %s written by a refused batch: %q, want %q", name, got, before[i])
		}
	}
	if len(r.changed) != 0 || len(r.plan.wrote) != 0 {
		t.Errorf("refused batch recorded writes: %v %v", r.changed, r.plan.wrote)
	}
	b.TestProposals[0].Reason = "two\nlines" // one bad side entry fails the whole batch before the bank is written
	answer, _ = json.Marshal(b)
	if err := r.applyAnswer(string(answer), io.Discard); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("a bad side entry did not stop the batch: %v", err)
	}
}

func TestDreamSideEntriesApplyWithBank(t *testing.T) {
	r, b, names := sideRollbackFixture(t)
	before := make([]string, len(names))
	for i, name := range names {
		before[i] = mustRead(t, name)
	}
	answer, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.applyAnswer(string(answer), io.Discard); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if got := mustRead(t, name); got == before[i] {
			t.Errorf("valid batch did not change %s", name)
		}
	}
	if !strings.Contains(mustRead(t, Path("conventions.md")), "New rule") {
		t.Error("bank update missing")
	}
}

func TestDreamValidateSideRejectsBadStrikeAndDuplicates(t *testing.T) {
	r, b, _ := sideRollbackFixture(t)
	task := b.PlanTasks[0]
	strike := func(reason, evidence string) Batch {
		return Batch{PlanTasks: []PlanTaskOp{{Plan: task.Plan, ID: task.ID, Action: "strike", Reason: reason, Evidence: evidence}}}
	}
	for name, bad := range map[string]Batch{
		"reason":    strike("bogus", "note:feedback/x.md"),
		"evidence":  strike("obsolete", "see notes"),
		"duplicate": {PlanTasks: []PlanTaskOp{task, task}},
		"dup-test":  {TestProposals: []testProposal{b.TestProposals[0], b.TestProposals[0]}},
	} {
		if err := r.validateSide(bad); err == nil {
			t.Errorf("%s: validateSide accepted the batch", name)
		}
	}
	if err := r.validateSide(strike("obsolete", "note:feedback/x.md")); err != nil {
		t.Errorf("valid strike rejected: %v", err)
	}
}

func TestDreamPreflightRefusesMalformedSideFile(t *testing.T) {
	r, b, _ := sideRollbackFixture(t)
	write(t, testProposalsFile(), "not json\n")
	answer, _ := json.Marshal(b)
	err := r.applyAnswer(string(answer), io.Discard)
	if err == nil || !refused(err) || !strings.Contains(err.Error(), "test proposals file") {
		t.Fatalf("want a preflight refusal naming the tests file: %v", err)
	}
	if !strings.Contains(mustRead(t, Path("conventions.md")), "Old rule") {
		t.Error("bank was written despite the refusal")
	}
}

func TestDreamSideFailureNamesAppliedAndNot(t *testing.T) {
	r, b, _ := sideRollbackFixture(t)
	if err := os.Remove(testProposalsFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(testProposalsFile(), 0o700); err != nil { // an unwritable destination: a real I/O failure
		t.Fatal(err)
	}
	err := r.applySide(b, io.Discard)
	want := "written: [candidate fc-a, plan task " + filepath.Base(b.PlanTasks[0].Plan) + ":Phase 1.1]; not written: [test proposal internal/x/empty input]"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("accounting missing: %v", err)
	}
}

func TestDreamPreflightRefusesUnresolvablePlanTask(t *testing.T) {
	r, b, _ := sideRollbackFixture(t)
	write(t, b.PlanTasks[0].Plan, "# P\n\n- [x] Phase 1.1 One (abc1234)\n")
	if err := r.preflightSide(b); err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("preflight accepted a closed task: %v", err)
	}
}

func TestDreamPeerCandidateWithSameIDIsNotOurs(t *testing.T) {
	r, b, _ := sideRollbackFixture(t)
	peer := b.Candidates[0]
	peer.Change = "A peer's change."
	line, _ := json.Marshal(peer)
	if err := proposeCandidate(io.Discard, string(line)); err != nil {
		t.Fatal(err)
	}
	err := r.applySide(Batch{Candidates: b.Candidates}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not written: [candidate fc-a]") {
		t.Fatalf("a peer's same-id record counted as ours: %v", err)
	}
}

// failedStore refuses every call with err.
type failedStore struct{ err error }

func (f failedStore) Load() (map[string]Doc, Versions, error) { return nil, nil, f.err }
func (f failedStore) Apply(Batch, Versions, time.Time) ([]OpResult, error) {
	return nil, f.err
}

func TestDreamUncertainCommitAttemptsNoSideEntry(t *testing.T) {
	r, b, names := sideRollbackFixture(t)
	before := make([]string, len(names))
	for i, name := range names {
		before[i] = mustRead(t, name)
	}
	unsure := &AfterCommitError{errors.New("roll back the write: failed")}
	old := OpenBankStore
	OpenBankStore = func(bool) (BankStore, func() error, error) {
		return failedStore{unsure}, func() error { return nil }, nil
	}
	t.Cleanup(func() { OpenBankStore = old })
	write(t, truthPath(), "db\n")
	err := r.applyChecked(b, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "side entries not attempted") || !strings.Contains(err.Error(), "candidate fc-a") {
		t.Fatalf("uncertain commit result: %v", err)
	}
	for i, name := range names {
		if got := mustRead(t, name); got != before[i] {
			t.Errorf("uncertain commit changed %s", name)
		}
	}
}

func TestDreamExtractUnderBudgetCallsNoModel(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	calls := 0
	old := CodeModeRun
	CodeModeRun = func(context.Context, string, string, string, string) (string, string, error) {
		calls++
		return "", "", nil
	}
	t.Cleanup(func() { CodeModeRun = old })
	var out strings.Builder
	dreamExtract(dreamOptions{Out: &out}, nil)
	if calls != 0 || out.Len() != 0 {
		t.Fatalf("under budget: calls=%d out=%q", calls, out.String())
	}
}

func TestDreamExtractOverBudgetConsolidatesOnce(t *testing.T) {
	for _, withLog := range []bool{true, false} {
		t.Run(fmt.Sprint("log=", withLog), func(t *testing.T) { dreamExtractOverBudget(t, withLog) })
	}
}

func dreamExtractOverBudget(t *testing.T, withLog bool) {
	isolatedConfig(t)
	initStore(t)
	root := paths.MemoryStoreDir()
	extractQuietRepo(t, storeRepo{Dir: root})
	if withLog {
		dreamWriteAccessLog(t)
	}
	dreamSeedHandoffs(t, root)
	calls := 0
	dreamExtractFake(t, &calls)
	var out strings.Builder
	dreamExtract(dreamOptions{Out: &out}, nil)
	want, wantCalls := "extract handoffs consolidated", 1
	if !withLog {
		want, wantCalls = ErrExtractNoAccessLog.Error(), 0
	}
	if calls != wantCalls || !strings.Contains(out.String(), want) {
		t.Fatalf("withLog=%v: calls=%d out=%q", withLog, calls, out.String())
	}
}

func dreamSeedHandoffs(t *testing.T, root string) {
	t.Helper()
	seedExtractHandoffs(t, root, time.Now())
	var rels []string
	for i := range 62 {
		rels = append(rels, fmt.Sprintf("handoffs/%03d.md", i))
	}
	if _, committed, err := (storeRepo{Dir: root}).Commit(rels, "seed", ""); err != nil || !committed {
		t.Fatalf("seed commit: committed=%v err=%v", committed, err)
	}
}

func dreamWriteAccessLog(t *testing.T) { dreamWriteAccessLogFor(t, "notes/other.md") }

func dreamWriteAccessLogFor(t *testing.T, entry string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(accessLogPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	line := time.Now().UTC().Format(time.RFC3339) + " -- " + entry + " -- " + AccessSearch + "\n"
	if err := os.WriteFile(accessLogPath(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dreamExtractFake answers an extract prompt with one entry that keeps every source's text.
func dreamExtractFake(t *testing.T, calls *int) {
	t.Helper()
	old := CodeModeRun
	CodeModeRun = func(_ context.Context, prompt, _, _, _ string) (string, string, error) {
		*calls++
		_, data, _ := strings.Cut(prompt, "Files: ")
		var files []extractPromptFile
		if err := json.Unmarshal([]byte(data), &files); err != nil {
			t.Fatal(err)
		}
		entry := ExtractEntry{Title: "Handoffs"}
		for _, f := range files {
			entry.Text += f.Text + "\n"
			entry.Sources = append(entry.Sources, f.Path)
		}
		answer, err := json.Marshal(ExtractRewriteAnswer{Entries: []ExtractEntry{entry}, Purge: []string{}})
		return string(answer), "fake-pi", err
	}
	t.Cleanup(func() { CodeModeRun = old })
}

func TestDreamApplyCheckedCountsAccessesDuringTheModelCall(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := paths.MemoryStoreDir()
	extractQuietRepo(t, storeRepo{Dir: root})
	dreamSeedHandoffs(t, root)
	dreamWriteAccessLogFor(t, "handoffs/000.md")
	group := ExtractGroup{Action: ExtractRewrite, Paths: []string{"handoffs/000.md", "handoffs/001.md"}}
	seen, err := dreamReadSources(root, group.Paths)
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "H", Text: "handoff 1: distinct facts", Sources: []string{"handoffs/001.md"}}}, Purge: []string{"handoffs/000.md"}}
	opts := ExtractOptions{Now: time.Now().Add(-time.Hour), AccessLogs: []string{accessLogPath()}}
	if _, err := dreamApplyChecked(root, group, ans, seen, opts); err == nil || !strings.Contains(err.Error(), "limit 2%") {
		t.Fatalf("a hit served after the snapshot time must count: %v", err)
	}
}

func TestRecordExtractLossAppendsAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "state", "extract-loss.log")
	now := time.Now()
	for _, lost := range []int{1, 2} {
		if err := RecordExtractLoss(ledger, now, lost); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(ledger)
	entries, _ := os.ReadDir(filepath.Dir(ledger))
	if strings.Count(string(data), "\n") != 2 || len(entries) != 1 {
		t.Fatalf("ledger %q, dir entries %d", data, len(entries))
	}
}

func TestTypedDreamAndUpdateRunWithoutLayoutStamp(t *testing.T) {
	isolatedConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	_ = os.Remove(stamp)
	calls := 0
	run := func(ctx context.Context, _, _ string) (string, error) {
		calls++
		if JobAnswerSchema(ctx) == "" {
			t.Error("run without a layout stamp did not request a typed answer")
		}
		AnswerJob(ctx, `{"ops":[{"op":"noop"}]}`)
		return "stub", nil
	}
	if err := dream(dreamOptions{CWD: t.TempDir(), Run: run, Forced: true}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) != layoutFlat { // writers restamp for peers
		t.Fatalf("stamp written back as %q, want %q", b, layoutFlat)
	}
	if err := update(updateOptions{CWD: t.TempDir(), Run: run, Session: "no-layout-stamp"}); err != nil {
		t.Fatal(err)
	}
	if result, ok := readUpdateResult("no-layout-stamp"); !ok || result.Typed != "ok" || calls != 2 {
		t.Errorf("update without a stamp: result=%+v ok=%v calls=%d", result, ok, calls)
	}
}

func TestRefuseDerivedSupersede(t *testing.T) {
	derived := Batch{Ops: []Op{{Op: "supersede", ID: contentRefPrefix + "abcdef12"}}}
	if err := refuseDerivedSupersede(derived); err == nil || !strings.Contains(err.Error(), "no stored id") {
		t.Errorf("derived supersede target = %v, want a refusal", err)
	}
	if err := refuseDerivedSupersede(Batch{Ops: []Op{{Op: "supersede", ID: "abcdef12"}, {Op: "close", ID: contentRefPrefix + "abcdef12"}}}); err != nil {
		t.Errorf("stored-id supersede and derived close refused: %v", err)
	}
}
