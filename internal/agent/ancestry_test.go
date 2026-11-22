package agent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestNearestDispatchCombinesBatchSiblings(t *testing.T) {
	setConfigDir(t)
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	const tokenCap = DefaultMaxTreeTokens / 2
	zero, two, three, tokens := 0, 2, 3, tokenCap
	p := mustPerms(t, "plan", []string{"Read"}, []string{"Bash(curl *)"}, []string{"Write"})
	q := mustPerms(t, "allow", []string{"Read", "Write"}, nil, []string{"Edit"})
	a := Record{ID: "a", PID: os.Getpid(), PIDStart: start, Status: "running", Depth: 2,
		MaxDepth: &three, MaxChildren: &two, MaxTreeTokens: &zero, ExportedPolicy: &p}
	b := Record{ID: "b", PID: a.PID, PIDStart: start, Status: "running",
		MaxDepth: &zero, MaxChildren: &zero, MaxTreeTokens: &tokens, ExportedPolicy: &q}
	old := b
	old.ID, old.PIDStart, old.Depth = "reused", "wrong-start", 99
	for _, recs := range [][]Record{{b, old, a}, {a, old, b}} {
		got, ok := nearestDispatch(recs, map[int]int{a.PID: 0})
		fields := []any{ok, got.ID, got.Depth, got.MaxDepth, got.MaxChildren, got.MaxTreeTokens}
		if !reflect.DeepEqual(fields, []any{true, "a", 2, &three, &two, &tokens}) {
			t.Fatalf("recovered lineage/caps = %+v", got)
		}
		policy := chainPolicy(got)
		decisions := []any{policy.Mode, policy.Decide("Write", "x"), policy.Decide("Edit", "x"), policy.Decide("Bash", "curl x")}
		if !reflect.DeepEqual(decisions, []any{"plan", permission.Deny, permission.Deny, permission.Ask}) {
			t.Fatalf("recovered policy = %+v", policy)
		}
		previous := ancestorDispatch
		ancestorDispatch = func() (Record, bool) { return got, true }
		values := withAncestorDispatch(proc.Values{maxDispatchDepthEnv: "0", maxDispatchChildrenEnv: "0", maxTreeTokensEnv: "0"})
		ancestorDispatch = previous
		caps := []string{values[maxDispatchDepthEnv], values[maxDispatchChildrenEnv], values[maxTreeTokensEnv]}
		if !reflect.DeepEqual(caps, []string{"3", "2", strconv.Itoa(tokens)}) {
			t.Fatalf("recovered caps = %v", values)
		}
	}
	if got := minDispatchCap(nil, &zero, DefaultMaxDispatchDepth); got != DefaultMaxDispatchDepth {
		t.Fatalf("legacy cap = %d", got)
	}
}

func TestChainPolicyKeepsExportedLaunchPolicy(t *testing.T) {
	hermetic(t, "")
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"fake":{"permission":{"defaults":{"deny":["Write"]}}}}}`)
	rec := NewRecord("fake", RunOptions{}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	req, err := checkRun(&fakeRunner{caps: Caps{Permissions: Native}}, rec.Request)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPermissionsEnv(withDispatchID(context.Background(), rec.ID, rec.Depth), req.Permissions)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"fake":{"permission":{"defaults":{}}}}}`)
	saved, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	var exported permission.Permissions
	values := proc.ValuesFromEnv((EnvSpec{Mode: EnvPrescrubbed}).Environ(ctx))
	if err := json.Unmarshal([]byte(values[DispatchPermissionsEnv]), &exported); err != nil {
		t.Fatal(err)
	}
	if got := chainPolicy(saved); !reflect.DeepEqual(got, exported) || got.Decide("Write", "x") != permission.Deny {
		t.Fatalf("recovered = %+v, exported = %+v", got, exported)
	}
	rec.finishSave(wire.Result{}, nil, 0, nil)
	saved, err = LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(chainPolicy(saved), exported) {
		t.Fatalf("finished snapshot = %+v, %v", saved.ExportedPolicy, err)
	}
	legacy := Record{ID: "legacy", Agent: "fake", Request: RunOptions{Permissions: req.Permissions}}
	if chainPolicy(legacy).Decide("Write", "x") != permission.Deny {
		t.Fatal("legacy request policy lost")
	}
}
