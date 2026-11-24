package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestProposalsParse(t *testing.T) {
	rep := "x\n```proposed_tests\n{\"pkg\":\"a\",\"case\":\"c1\",\"reason\":\"r\"}\n```\n"
	got, err := ParseTestProposals(rep, "reports/x.md")
	if err != nil || len(got) != 1 || got[0].Source != "reports/x.md" {
		t.Fatalf("parse = %v, %v", got, err)
	}
	if _, err := ParseTestProposals("```proposed_tests\n{\"pkg\":\"a\"}\n```", "s"); err == nil {
		t.Fatal("a line without case and reason must fail")
	}
}

func TestTestProposalsDedupeCap(t *testing.T) {
	f := filepath.Join(t.TempDir(), "techdebt", "tests.jsonl")
	one := []TestProposal{{Pkg: "a", Case: "c1", Reason: "r"}, {Pkg: "a", Case: "C1", Reason: "r"}}
	if n, err := AppendTestProposals(f, one); err != nil || n != 1 {
		t.Fatalf("first append kept %d, %v", n, err)
	}
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
