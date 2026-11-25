package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

const candidateRows = `{"id":"fc-a","evidence":["feedback/feedback-a.md","notes/note-c.md"],"repeats":1,"change":"Add a hook.","target":"unknown","kind":"hook","size":"small"}` + "\n" +
	`{"id":"fc-b","evidence":["feedback/feedback-b.md"],"repeats":3,"change":"Add a flag.","target":"internal/foo/a.go","kind":"verb","size":"medium"}`

func TestCandidateApproveAndClose(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	t.Setenv(dreamChildEnv, "")
	for _, n := range []string{"feedback/feedback-a.md", "feedback/feedback-b.md", "notes/note-c.md"} {
		write(t, filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(n)), "x\n")
	}
	write(t, filepath.Join(cfg, "rules", "x.md"), "one\ntwo\n")
	path := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel))
	write(t, path, "# C\n\n```jsonl\n"+candidateRows+"\n```\n")
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if got := openCandidatesNotice(); got != "2 open feedback candidates" {
		t.Errorf("notice = %q", got)
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-b", Enforced: "rules/x.md:2"}); err == nil {
		t.Error("enforced before approval was accepted")
	}
	for _, o := range []ReviewOptions{{Candidate: "fc-b", Approve: true}, {Candidate: "fc-b", Enforced: "rules/x.md:2"}} {
		if err := candidateAction(&out, o); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-a", Enforced: "rules/x.md:9"}); err == nil {
		t.Error("a line past the end of the file was accepted")
	}
	got := mustRead(t, path)
	if !strings.Contains(got, `"status":"enforced","enforced_by":"rules/x.md:2"`) || !strings.Contains(got, strings.SplitN(candidateRows, "\n", 2)[0]) {
		t.Errorf("record not rewritten in place:\n%s", got)
	}
	if p := candidateProblems(); len(p) != 0 || openCandidatesNotice() != "1 open feedback candidate" {
		t.Errorf("problems %v, notice %q", p, openCandidatesNotice())
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-zz"}); err != errNoCandidate {
		t.Errorf("unknown id: %v", err)
	}
	t.Setenv(dreamChildEnv, "1")
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-a", Approve: true}); err == nil {
		t.Error("a dream job approved a candidate")
	}
}

// A target with shell metacharacters fails validation, and the printed gate is a quoted clean package path.
func TestCandidateTargetNeverReachesShell(t *testing.T) {
	isolatedConfig(t)
	for _, target := range []string{"internal/x/$(touch /tmp/pwned).go", "internal/x;id", "internal/a b/c.go", "internal/x'y/z.go",
		"internal/`id`/z.go", "-internal/x.go", "internal/-rf/x.go", "internal/../x.go", "internal/x\ny/z.go"} {
		if problem := targetProblem(target); problem == "" || strings.Contains(problem, "touch") {
			t.Errorf("target %q: problem %q", target, problem)
		}
		if g := candidateGate(target); g != "" {
			t.Errorf("target %q: gate %q", target, g)
		}
	}
	if g := candidateGate("internal/foo/a.go"); targetProblem("internal/foo/a.go") != "" || !strings.HasSuffix(g, "go test ./internal/foo/...") {
		t.Errorf("plain target: gate %q", g)
	}
}

func TestCandidatePropose(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), "feedback", "feedback-a.md"), "x\n")
	write(t, filepath.Join(paths.MemoryStoreDir(), "notes", "note-c.md"), "x\n")
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := strings.Cut(candidateRows, "\n")
	var out strings.Builder
	if err := proposeCandidate(&out, rec); err != nil {
		t.Fatal(err)
	}
	if err := proposeCandidate(&out, rec); err == nil {
		t.Error("a duplicate id was appended")
	}
	if err := proposeCandidate(&out, strings.Replace(rec, `"fc-a"`, `"fc-z","status":"approved"`, 1)); err == nil {
		t.Error("a proposal with a status was accepted")
	}
	if p := candidateProblems(); len(p) != 0 || openCandidatesNotice() != "1 open feedback candidate" {
		t.Errorf("problems %v, notice %q", p, openCandidatesNotice())
	}
}

func TestTestProposalsParse(t *testing.T) {
	rep := "x\n```proposed_tests\n{\"pkg\":\"a\",\"case\":\"c1\",\"reason\":\"r\"}\n```\n"
	got, err := parseTestProposals(rep, "reports/x.md")
	if err != nil || len(got) != 1 || got[0].Source != "reports/x.md" {
		t.Fatalf("parse = %v, %v", got, err)
	}
	for _, bad := range []string{`{"pkg":"a"}`, `{"pkg":"a","case":"c","reason":"r"}{"x":1}`} {
		if _, err := parseTestProposals("```proposed_tests\n"+bad+"\n```", "s"); err == nil {
			t.Fatalf("%s must fail", bad)
		}
	}
}

func TestTestProposalsKeepValidLines(t *testing.T) {
	got, err := parseTestProposals("```proposed_tests\n{bad\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\n```", "s")
	if err == nil || len(got) != 1 {
		t.Fatalf("got %v, err %v: want the valid line kept and the bad one reported", got, err)
	}
}

func TestTestProposalsDedupeCap(t *testing.T) {
	f := filepath.Join(t.TempDir(), "techdebt", "tests.jsonl")
	one := []testProposal{{Pkg: "a", Case: "c1", Reason: "r"}, {Pkg: "a", Case: " c1", Reason: "r"}}
	if n, err := appendTestProposals(f, one); err != nil || n != 1 {
		t.Fatalf("first append kept %d, %v", n, err)
	}
	if err := os.WriteFile(f, []byte("{bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := appendTestProposals(f, one); err == nil {
		t.Fatal("a malformed existing line must fail, not be erased")
	}
	_ = os.Remove(f)
	var many []testProposal
	for i := range 50 {
		many = append(many, testProposal{Pkg: "p", Case: strings.Repeat("c", i+1), Reason: "r"})
	}
	n, err := appendTestProposals(f, many)
	b, _ := os.ReadFile(f)
	if err != nil || n != maxTestProposals || strings.Count(string(b), "\n") != maxTestProposals {
		t.Fatalf("capped append kept %d (%v), lines %d", n, err, strings.Count(string(b), "\n"))
	}
}

func TestIngestTestProposalsFromScratchReports(t *testing.T) {
	isolatedConfig(t)
	if err := os.MkdirAll(paths.MemorySubDir("reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# r\n```proposed_tests\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\n```\n"
	if err := os.WriteFile(paths.MemorySub("reports", "x.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for range 2 {
		if err := ingestTestProposals(&out); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(testProposalsFile())
	if strings.Count(string(b), "\n") != 1 || !strings.Contains(string(b), `"source":"reports/x.md"`) {
		t.Fatalf("list = %q, out %q", b, out.String())
	}
	// a report written after the watermark (as during settlement) is picked up by the next ingest
	late := strings.Replace(body, `"case":"c"`, `"case":"late"`, 1)
	if err := os.WriteFile(paths.MemorySub("reports", "y.md"), []byte(late), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	_ = os.Chtimes(paths.MemorySub("reports", "y.md"), future, future)
	if err := ingestTestProposals(&out); err != nil {
		t.Fatal(err)
	}
	if b, _ = os.ReadFile(testProposalsFile()); !strings.Contains(string(b), `"case":"late"`) {
		t.Fatalf("late report not ingested: %q", b)
	}
}

func TestProposalFenceNestedIsSkipped(t *testing.T) {
	nested := "````markdown\n```proposed_tests\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\n```\n````\n"
	if got, err := parseTestProposals(nested, "s"); err != nil || len(got) != 0 {
		t.Fatalf("nested fence ingested: %v, %v", got, err)
	}
	other := "```c++\nint x;\n```\r\n```proposed_tests\r\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\r\n```\r\n"
	if got, err := parseTestProposals(other, "s"); err != nil || len(got) != 1 {
		t.Fatalf("after a c++ fence with CRLF: %v, %v", got, err)
	}
}

func TestAppendTestProposalsLeavesNoLockInStore(t *testing.T) {
	isolatedConfig(t)
	f := filepath.Join(t.TempDir(), "techdebt", "tests.jsonl")
	if _, err := appendTestProposals(f, []testProposal{{Pkg: "a", Case: "c", Reason: "r"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(f), "*.lock")); len(m) != 0 {
		t.Fatalf("lock files beside tests.jsonl: %v", m)
	}
}

// Dream lists feedback notes, its seam writes the candidates file; review accepts it and rejects each bad variant.
func TestCandidatesEndToEnd(t *testing.T) {
	isolatedConfig(t)
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
	root := repoRoot(t, "proj")
	for _, n := range []string{"feedback/feedback-a.md", "feedback/feedback-b.md", "notes/note-c.md"} {
		write(t, filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(n)), "restated twice\n")
	}
	candidates := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel))
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
	write(t, filepath.Join(paths.MemoryStoreDir(), "feedback", "feedback-a.md"), "x\n")
	write(t, filepath.Join(paths.MemoryStoreDir(), "notes", "note-c.md"), "x\n")
	for _, l := range [][2]string{{outside, filepath.Join(cfg, "linkout")}, {outside, filepath.Join(paths.MemoryStoreDir(), "knowledge-out")},
		{filepath.Join(paths.MemoryStoreDir(), "feedback", "feedback-a.md"), filepath.Join(paths.MemoryStoreDir(), "feedback", "alias.md")},
		{filepath.Join(paths.MemoryStoreDir(), "feedback"), filepath.Join(paths.MemoryStoreDir(), "notes-alias")}} {
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
	assertCandidateCaseAlias(t)
}

// The change cap counts characters, not bytes.
func TestOneSentenceCountsRunes(t *testing.T) {
	if !oneSentence(strings.Repeat("é", candidateMaxText)) || oneSentence(strings.Repeat("é", candidateMaxText+1)) {
		t.Error("the cap is not 200 runes")
	}
}

func assertCandidateCaseAlias(t *testing.T) {
	t.Helper()
	if upper := filepath.Join(paths.MemoryStoreDir(), "feedback", "FEEDBACK-A.md"); paths.FoldsCase {
		if _, err := os.Stat(upper); err == nil && len(evidenceProblems(candidate{Evidence: []string{"feedback/feedback-a.md", "feedback/FEEDBACK-A.md"}})) == 0 {
			t.Error("two spellings of one file counted as two")
		}
	}
}
