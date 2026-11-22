package agent

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCheckAncestorDispatchUsesInProcessIdentity(t *testing.T) {
	previous := ancestorDispatch
	ancestorDispatch = func() (Record, error) { return Record{}, os.ErrPermission }
	t.Cleanup(func() { ancestorDispatch = previous })
	ctx := proc.WithValues(t.Context(), proc.Values{})
	if err := CheckAncestorDispatch(withDispatchID(ctx, "task", 2)); err != nil {
		t.Fatalf("explicit in-process dispatch used ancestry: %v", err)
	}
	if err := CheckAncestorDispatch(ctx); err == nil {
		t.Fatal("unbound context bypassed ancestry refusal")
	}
}

func TestNearestDispatchRefusesBatchSiblings(t *testing.T) {
	setConfigDir(t)
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a := Record{ID: "a", PID: os.Getpid(), PIDStart: start, Status: "running"}
	b := Record{ID: "b", PID: a.PID, PIDStart: start, Status: "running"}
	old := b
	old.ID, old.PIDStart = "reused", "wrong-start"
	for _, recs := range [][]Record{{b, old, a}, {a, old, b}} {
		got, err := nearestDispatch(recs, map[int]int{a.PID: 0})
		if err == nil || got.ID != "" || !strings.Contains(err.Error(), strconv.Itoa(a.PID)) {
			t.Fatalf("ambiguous parent = %+v, %v", got, err)
		}
		previous := ancestorDispatch
		ancestorDispatch = func() (Record, error) { return got, err }
		values := withAncestorDispatch(proc.Values{})
		refusal := CheckAncestorDispatch(context.Background())
		ancestorDispatch = previous
		if values[proc.DispatchIDEnv] != "" || refusal == nil {
			t.Fatalf("ambiguous parent exported id %q, refusal %v", values[proc.DispatchIDEnv], refusal)
		}
	}
	if got, err := nearestDispatch([]Record{a, old}, map[int]int{a.PID: 0}); err != nil || got.ID != "a" {
		t.Fatalf("unique parent = %+v, %v", got, err)
	}
}

func TestNearestDispatchRejectsPoolHistory(t *testing.T) {
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	a := Record{ID: "active", PID: os.Getpid(), PIDStart: start, Status: "running"}
	finished, batch := a, a
	finished.ID, finished.Status = "finished", "ok"
	batch.BatchID = "pool"
	for _, recs := range [][]Record{{a, finished}, {finished, a}, {finished}, {batch}} {
		if got, err := nearestDispatch(recs, map[int]int{a.PID: 0}); err == nil || got.ID != "" {
			t.Fatalf("pool history %v selected %q without refusal: %v", recs, got.ID, err)
		}
	}
	finished.PIDStart = "reused-pid"
	if got, err := nearestDispatch([]Record{finished, a}, map[int]int{a.PID: 0}); err != nil || got.ID != a.ID {
		t.Fatalf("unrelated process history blocked unique dispatch: %q, %v", got.ID, err)
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
