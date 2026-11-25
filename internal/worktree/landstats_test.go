package worktree

import (
	"os"
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
