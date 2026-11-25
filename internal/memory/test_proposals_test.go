package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestTestProposalsParse(t *testing.T) {
	rep := "x\n```proposed_tests\n{\"pkg\":\"a\",\"case\":\"c1\",\"reason\":\"r\"}\n```\n"
	got, err := ParseTestProposals(rep, "reports/x.md")
	if err != nil || len(got) != 1 || got[0].Source != "reports/x.md" {
		t.Fatalf("parse = %v, %v", got, err)
	}
	for _, bad := range []string{`{"pkg":"a"}`, `{"pkg":"a","case":"c","reason":"r"}{"x":1}`} {
		if _, err := ParseTestProposals("```proposed_tests\n"+bad+"\n```", "s"); err == nil {
			t.Fatalf("%s must fail", bad)
		}
	}
}

func TestTestProposalsKeepValidLines(t *testing.T) {
	got, err := ParseTestProposals("```proposed_tests\n{bad\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\n```", "s")
	if err == nil || len(got) != 1 {
		t.Fatalf("got %v, err %v: want the valid line kept and the bad one reported", got, err)
	}
}

func TestTestProposalsDedupeCap(t *testing.T) {
	f := filepath.Join(t.TempDir(), "techdebt", "tests.jsonl")
	one := []TestProposal{{Pkg: "a", Case: "c1", Reason: "r"}, {Pkg: "a", Case: " c1", Reason: "r"}}
	if n, err := AppendTestProposals(f, one); err != nil || n != 1 {
		t.Fatalf("first append kept %d, %v", n, err)
	}
	if err := os.WriteFile(f, []byte("{bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendTestProposals(f, one); err == nil {
		t.Fatal("a malformed existing line must fail, not be erased")
	}
	_ = os.Remove(f)
	var many []TestProposal
	for i := range 50 {
		many = append(many, TestProposal{Pkg: "p", Case: strings.Repeat("c", i+1), Reason: "r"})
	}
	n, err := AppendTestProposals(f, many)
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
	if got, err := ParseTestProposals(nested, "s"); err != nil || len(got) != 0 {
		t.Fatalf("nested fence ingested: %v, %v", got, err)
	}
	other := "```c++\nint x;\n```\r\n```proposed_tests\r\n{\"pkg\":\"a\",\"case\":\"c\",\"reason\":\"r\"}\r\n```\r\n"
	if got, err := ParseTestProposals(other, "s"); err != nil || len(got) != 1 {
		t.Fatalf("after a c++ fence with CRLF: %v, %v", got, err)
	}
}

func TestAppendTestProposalsLeavesNoLockInStore(t *testing.T) {
	isolatedConfig(t)
	f := filepath.Join(t.TempDir(), "techdebt", "tests.jsonl")
	if _, err := AppendTestProposals(f, []TestProposal{{Pkg: "a", Case: "c", Reason: "r"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(f), "*.lock")); len(m) != 0 {
		t.Fatalf("lock files beside tests.jsonl: %v", m)
	}
}
