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
	ingestTestProposals(time.Time{}, &out)
	ingestTestProposals(time.Time{}, &out)
	b, _ := os.ReadFile(testProposalsFile())
	if strings.Count(string(b), "\n") != 1 || !strings.Contains(string(b), `"source":"reports/x.md"`) {
		t.Fatalf("list = %q, out %q", b, out.String())
	}
}
