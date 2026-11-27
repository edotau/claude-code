package inspect

import (
	"testing"

	"github.com/edotau/claude-code/internal/platform/contract"
)

func TestViewContractTerms(t *testing.T) {
	c := contract.Contract{ID: "j", Terms: []contract.Term{contract.TermGate, contract.TermTests},
		Records: []contract.TermRecord{{Term: contract.TermGate, Verdict: contract.VerdictPass, By: contract.ByGo, Evidence: "0"}}}
	v := ViewContract(c, contract.Spent{Tokens: 5})
	if len(v.Terms) != 2 || v.Terms[0].Verdict != "pass" || v.Terms[1].Verdict != "pending" || v.Spent.Tokens != 5 {
		t.Errorf("view = %+v", v)
	}
	if v.Note() != "unmet tests" {
		t.Errorf("note = %q", v.Note())
	}
}

func TestContractViewNoteLeaves(t *testing.T) {
	c := contract.Contract{ID: "j", Terms: []contract.Term{contract.TermLeaves}, Limits: contract.Limits{Leaves: 2}}
	v := ViewContract(c, contract.Spent{Leaves: 2})
	if v.Note() != "leaves 2/2 unmet leaves" {
		t.Errorf("note = %q", v.Note())
	}
}
