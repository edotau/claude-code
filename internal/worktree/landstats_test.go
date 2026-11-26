package worktree

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func att(lane string, n int, at int64, out Outcome, secs float64) LandAttempt {
	return LandAttempt{Repo: "/r", Lane: lane, Attempt: n, Time: at, Outcome: out, Seconds: StageSeconds{Test: secs}}
}

func TestRecordAttemptAppendsOneLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	RecordAttempt(att("a", 1, 100, OutcomeTargetMoved, 1))
	RecordAttempt(att("a", 2, 110, OutcomeLanded, 2))
	raw, err := os.ReadFile(LandAttemptsFile())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "\n"); got != 2 {
		t.Fatalf("lines = %d, want 2:\n%s", got, raw)
	}
	if parsed := parseAttempts(raw); len(parsed) != 2 || parsed[0].Schema != landAttemptSchema || parsed[1].Attempt != 2 {
		t.Fatalf("parsed %+v", parsed)
	}
}

func TestRecordAttemptFailureIsNotFatal(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(cfg+"/state", nil, 0o600); err != nil { // a file where the state dir must go
		t.Fatal(err)
	}
	RecordAttempt(att("a", 1, 100, OutcomeLanded, 1))
}

func TestTrimAttemptsKeepsNewest(t *testing.T) {
	file := t.TempDir() + "/l.jsonl"
	if err := os.WriteFile(file, []byte("1\n2\n3\n4\n5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trimAttempts(file, 2); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "4\n5\n" {
		t.Fatalf("after trim %q", raw)
	}
	if err := trimAttempts(file, 5); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "4\n5\n" {
		t.Fatalf("a short file changed: %q", raw)
	}
}

func TestSummarizeLands(t *testing.T) {
	all := []LandAttempt{
		att("a", 1, 100, OutcomeTargetMoved, 10),
		att("b", 1, 105, OutcomeGateFailed, 3), // never lands: not a land
		att("a", 2, 130, OutcomeLanded, 20),    // 130-100+20 = 50s, two attempts, moved
		att("c", 1, 200, OutcomeLanded, 10),    // 10s, one attempt
		att("d", 1, 10, OutcomeLanded, 999),    // before the window
	}
	all[3].Reval = RevalReused
	s := summarize(all, "/r", 50)
	if s.Lands != 2 || s.P50 != 10*time.Second || s.P95 != 50*time.Second {
		t.Fatalf("summary %+v", s)
	}
	if s.AttemptsPerLand != 1.5 || s.MovedShare != 0.5 || s.Reused != 1 {
		t.Fatalf("summary %+v", s)
	}
	if got := s.Line(7 * 24 * time.Hour); got != "p50 10s p95 50s · 1.5 attempts/land · moved 50% · 2 lands (7d)" {
		t.Fatalf("line %q", got)
	}
	if other := summarize(all, "/other", 0); other.Lands != 0 || other.Line(24*time.Hour) != "no lands (1d)" {
		t.Fatalf("other repo %+v", other)
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		9 * time.Second: "9s", 3*time.Minute + 10*time.Second: "3m10s", 14 * time.Minute: "14m",
	} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%v) = %q, want %q", d, got, want)
		}
	}
}

// A failing gate records the packages it named; a scope records the changed and importer-only packages apart.
func TestLandAttemptRecordsScopeAndFailedPkgs(t *testing.T) {
	rec := &attempt{}
	rec.scoped(gateScope{Changed: map[string]bool{"x/a": true}, Tested: map[string]bool{"x/a": true, "x/c": true, "x/b": true}})
	if got := rec.a; got.Packages != 3 || strings.Join(got.ChangedPkgs, ",") != "x/a" || strings.Join(got.ImporterPkgs, ",") != "x/b,x/c" {
		t.Fatalf("record = %+v", got)
	}
	var out, errOut bytes.Buffer
	gate := "echo 'FAIL\tm/p1\t0.1s'; echo '# m/p2' >&2; echo './p.go:3:4: undefined: y' >&2; exit 1"
	req := LandRequest{Out: &out, Err: &errOut}
	if err := runGate(t.Context(), req, gateAt{Dir: t.TempDir()}, gate, "flag", rec); err == nil {
		t.Fatal("gate passed, want failure")
	}
	if got := strings.Join(rec.a.FailedPkgs, ","); got != "m/p1,m/p2" {
		t.Fatalf("failed_pkgs = %q, want m/p1,m/p2", got)
	}
}

// Only a FAIL line or a header followed by an error line fails a package; a warning or a bare header does not.
func TestRunGateFailedPkgsNeedErrorLines(t *testing.T) {
	rec := &attempt{}
	gate := "printf '# m/cgo\\nw.c:1:2: warning: unused\\n# m/bad\\n./b.go:3:4: undefined: y\\n# m/lone\\n' >&2; " +
		"echo 'FAIL\tm/t\t0.1s'; exit 1"
	req := LandRequest{Out: io.Discard, Err: io.Discard}
	if err := runGate(t.Context(), req, gateAt{Dir: t.TempDir()}, gate, "flag", rec); err == nil {
		t.Fatal("gate passed, want failure")
	}
	got := slices.Sorted(slices.Values(rec.a.FailedPkgs))
	if strings.Join(got, ",") != "m/bad,m/t" {
		t.Fatalf("failed_pkgs = %v, want m/bad,m/t", got)
	}
}

// A first gate that fails still leaves the changed and importer scope in the attempt.
func TestLandFailedFirstGateRecordsScope(t *testing.T) {
	repo, main, _ := laneLandFixture(t)
	landFixtureWrite(t, main, "go.mod", "module example.com/m\n\ngo 1.22\n", "mod")
	landFixtureWrite(t, main, "Makefile", "test:\n\ttrue\n", "make")
	wt, err := Create(repo, "c", CreateOptions{Session: "session-c", Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "internal/foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	landFixtureWrite(t, wt, "internal/foo/foo.go", "package foo\n", "foo")
	landFixtureWrite(t, wt, "internal/foo/foo_test.go", "package foo\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { t.Fatal(\"x\") }\n", "foo test")
	var out bytes.Buffer
	req := laneRequest("c", &out)
	req.NoVerify = false
	if _, err := Land(t.Context(), repo, req); err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("Land = %v, want verify failed\n%s", err, out.String())
	}
	got := readAttempts(t)
	if len(got) == 0 || strings.Join(got[len(got)-1].ChangedPkgs, ",") != "internal/foo" {
		t.Fatalf("attempts %+v, want changed_pkgs internal/foo\n%s", got, out.String())
	}
}
