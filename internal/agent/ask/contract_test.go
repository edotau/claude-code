package ask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/worktree"
)

// openSaved opens and saves contract d-1 for f under a scratch config dir.
func openSaved(t *testing.T, f contract.Facts, dir, scope string) contract.Contract {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c, ps, err := openContract(contractOpen{
		Scope: contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		Owner: contract.Party{Role: contract.RoleOwner, Agent: "pi"}, Limits: contract.Limits{Depth: 1, Leaves: 1},
		Facts: f, Dispatch: "d-1", Dir: dir, ProxyScope: scope})
	if len(ps) > 0 || err != nil {
		t.Fatal(ps, err)
	}
	return c
}

func scratchFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenContractObservesLaneAndProxy: a plain dir is no lane, an existing scope file passes proxy; abandon fails it.
func TestOpenContractObservesLaneAndProxy(t *testing.T) {
	dir, scope := t.TempDir(), scratchFile(t, "scope.json")
	c := openSaved(t, contract.Facts{Lane: true, ScopedProxy: true}, dir, scope)
	got := [][2]string{}
	for _, r := range c.Records {
		got = append(got, [2]string{string(r.Term), string(r.Verdict) + " " + r.Evidence})
	}
	want := [][2]string{{"lane", "fail " + missAbsent}, {"proxy", "pass " + scope}}
	if !reflect.DeepEqual(got, want) || c.State() != contract.StateWorking {
		t.Fatalf("state %s, open records %v; want working, %v", c.State(), got, want)
	}
	if a, err := abandonContract("d-1", contract.ReasonCancelled); err != nil || a.State() != contract.StateFailed {
		t.Fatalf("abandon: state %s, %v; want failed", a.State(), err)
	}
}

// TestSettleContractFailsOnRedGate: a red gate fails the saved contract (reason term); the rest pass.
func TestSettleContractFailsOnRedGate(t *testing.T) {
	openSaved(t, contract.Facts{}, "", "")
	one := 1
	got, err := settleContract("d-1", contractReturn{End: endFinished, Gate: &worktree.GateRun{Exit: &one},
		Verify:   &wire.VerifyResult{Exit: 0},
		Handback: json.RawMessage(`{"status":"DONE","summary":"s","unfixed":[],"tests":"judged","method":"solo"}`),
		Report:   scratchFile(t, "r.md")})
	if err != nil {
		t.Fatal(err)
	}
	if got.State() != contract.StateFailed || got.Failed != contract.ReasonTerm ||
		!reflect.DeepEqual(got.Unmet(), []contract.Term{contract.TermGate}) {
		t.Fatalf("state %s failed %q unmet %v; want failed, term, [gate]", got.State(), got.Failed, got.Unmet())
	}
}
