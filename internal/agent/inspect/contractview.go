package inspect

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/contract"
	"github.com/edotau/claude-code/internal/platform/redact"
)

// ContractView is a dispatch's contract as `agents status --format json` shows it.
type ContractView struct {
	ID     string          `json:"id"`
	Kind   contract.Kind   `json:"kind"`
	Role   contract.Role   `json:"role"`
	State  string          `json:"state"`
	Failed contract.Reason `json:"failed,omitempty"`
	Terms  []TermView      `json:"terms"`
	Spent  contract.Spent  `json:"spent"`
	Limits contract.Limits `json:"limits"`
	Unmet  []contract.Term `json:"unmet,omitempty"`
}

// Note is the status row's contract text: the leaves spent of the limit, and the terms still unmet.
func (v ContractView) Note() string {
	var parts []string
	if v.Limits.Leaves > 0 {
		parts = append(parts, fmt.Sprintf("leaves %d/%d", v.Spent.Leaves, v.Limits.Leaves))
	}
	if len(v.Unmet) > 0 {
		names := make([]string, len(v.Unmet))
		for i, t := range v.Unmet {
			names[i] = string(t)
		}
		parts = append(parts, "unmet "+strings.Join(names, ","))
	}
	return strings.Join(parts, " ")
}

// TermView is one term with its last record's verdict, or pending.
type TermView struct {
	Term     contract.Term `json:"term"`
	Verdict  string        `json:"verdict"` // pass, fail or pending
	By       string        `json:"by,omitempty"`
	Evidence string        `json:"evidence,omitempty"`
}

// ViewContract is the typed view of c; spent is what its subtree used.
func ViewContract(c contract.Contract, spent contract.Spent) ContractView {
	v := ContractView{ID: c.ID, Kind: c.Scope.Kind, Role: c.Owner.Role, State: string(c.State()), Failed: c.Failed,
		Terms: []TermView{}, Spent: spent, Limits: c.Limits}
	if c.State() != contract.StateClosed {
		v.Unmet = c.Unmet()
	}
	for _, t := range c.Terms {
		tv := TermView{Term: t, Verdict: "pending"}
		for _, r := range c.Records {
			if r.Term == t {
				tv.Verdict, tv.By, tv.Evidence = string(r.Verdict), r.By, redact.Text(r.Evidence)
			}
		}
		v.Terms = append(v.Terms, tv)
	}
	return v
}

// ContractOf loads id's contract view; nil when the dispatch holds none.
func ContractOf(id string) (*ContractView, error) {
	c, err := contract.Load(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := ViewContract(c, wire.SpentBy(c))
	return &v, nil
}

// StatusRow is an agent.Row with its contract view.
type StatusRow struct {
	agent.Row
	Contract *ContractView `json:"contract,omitempty"`
}
