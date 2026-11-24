package ask

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// openSaved opens and saves contract d-1 for f.
func openSaved(t *testing.T, f contract.Facts, dir, scope string) contract.Contract {
	t.Helper()
	c, ps, err := openContract(contractOpen{
		Scope: contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}, Lane: "l"},
		Owner: contract.Party{Role: contract.RoleOwner, Agent: "pi"}, Limits: contract.Limits{Depth: 1, Leaves: 1},
		Facts: f, Dispatch: "d-1", Dir: dir, ProxyScope: scope})
	if len(ps) > 0 || err != nil {
		t.Fatal(ps, err)
	}
	return c
}

// scratchFile writes dir/name under a scratch config dir that this test now uses.
func scratchFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOpenContractObservesLaneAndProxy: a plain dir is no lane, a scope file passes proxy; abandon fails it.
func TestOpenContractObservesLaneAndProxy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, scope := t.TempDir(), scratchFile(t, filepath.Join(paths.StateDir(), "proxy", "scopes"), "g.json")
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

// TestStartAndSettleAsk: startAsk opens a working contract; settleHeld with no gate and no hand-back fails it on term.
func TestStartAndSettleAsk(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	o := &contractOpen{
		Scope:  contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.go"}},
		Owner:  contract.Party{Role: contract.RoleOwner, Agent: "pi"},
		Limits: contract.Limits{Depth: 1, Leaves: 1}}
	ctx := withContract(context.Background(), o)
	rec := agent.Record{ID: "d-9", Status: wire.StatusOK, Time: time.Now()}
	if err := startAsk(ctx, nil, &rec); err != nil {
		t.Fatal(err)
	}
	if c, err := contract.Load("d-9"); err != nil || c.State() != contract.StateWorking {
		t.Fatalf("after start: %v, %v; want working", c.State(), err)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	settleHeld(ctx, &agent.Prepared{Record: rec}, wire.Result{Status: wire.StatusOK}, nil)
	if c, err := contract.Load("d-9"); err != nil || c.State() != contract.StateFailed || c.Failed != contract.ReasonTerm {
		t.Fatalf("after settle: %v %q, %v; want failed term", c.State(), c.Failed, err)
	}
}

// TestSettleContractFailsOnRedGate: a red gate fails the saved contract (reason term); the rest pass.
func TestSettleContractFailsOnRedGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	openSaved(t, contract.Facts{}, "", "")
	one := 1
	got, err := settleContract("d-1", contractReturn{End: endFinished, Gate: &worktree.GateRun{Exit: &one},
		Verify:   &wire.VerifyResult{Exit: 0},
		Handback: &wire.Handback{Status: "DONE", Summary: "s", Unfixed: []string{}},
		Tests:    contract.TestsJudged, Method: contract.MethodSolo, Report: scratchFile(t, paths.MemorySubDir("reports"), "r.md")})
	if err != nil {
		t.Fatal(err)
	}
	if got.State() != contract.StateFailed || got.Failed != contract.ReasonTerm ||
		!reflect.DeepEqual(got.Unmet(), []contract.Term{contract.TermGate}) {
		t.Fatalf("state %s failed %q unmet %v; want failed, term, [gate]", got.State(), got.Failed, got.Unmet())
	}
}
