package agent

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/proxy/proxyteam"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/route"
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

func TestAncestryRestoresFenceAndSkipsWaves(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay) // replay admits only a registered parent row
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	leaf := Record{ID: "leaf", PID: os.Getpid(), PIDStart: start, Status: "running",
		Route: route.Route{Provider: "databricks", Profile: "qa"}}
	waves := Record{ID: "coord", Agent: wavesJobAgent, PID: leaf.PID, PIDStart: start, Status: "running"}
	if got, err := nearestDispatch([]Record{waves}, map[int]int{waves.PID: 0}); err != nil || got.ID != "" {
		t.Fatalf("waves coordinator taken as parent: %q, %v", got.ID, err)
	}
	got, err := nearestDispatch([]Record{waves, leaf}, map[int]int{leaf.PID: 0})
	if err != nil || got.ID != leaf.ID {
		t.Fatalf("parent = %q, %v", got.ID, err)
	}
	previous := ancestorDispatch
	ancestorDispatch = func() (Record, error) { return got, nil }
	t.Cleanup(func() { ancestorDispatch = previous })
	values := withAncestorDispatch(proc.Values{route.OverlayVar: `{"provider":"anthropic"}`})
	assertQAFence(t, values[route.OverlayVar])
}

// assertQAFence checks doc is the databricks qa unit fenced to qa, whatever else the schema holds.
func assertQAFence(t *testing.T, doc string) {
	t.Helper()
	o, err := agentconfig.LoadOverlayContext(t.Context(), doc, nil)
	if err != nil || o.Provider != "databricks" || o.Profile != "qa" || o.Fence == nil || len(o.Fence.Profiles) != 1 || o.Fence.Profiles[0] != "qa" {
		t.Fatalf("fence = %q (%v), want databricks qa fenced to qa", doc, err)
	}
}

// An env-wiped ask carries the recovered parent fence in its context before any route check reads it.
func TestAncestryContextInstallsFence(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(route.OverlayVar, `{"provider":"anthropic"}`)
	previous := ancestorDispatch
	ancestorDispatch = func() (Record, error) {
		return Record{ID: "leaf", Status: "running", Route: route.Route{Provider: "databricks", Profile: "qa"}}, nil
	}
	t.Cleanup(func() { ancestorDispatch = previous })
	ctx := AncestorContext(context.Background())
	assertQAFence(t, proc.Value(ctx, route.OverlayVar))
}

// A parent whose row is not registered still confines its child: the fence admits no profile at all.
func TestAncestryUnregisteredParentFailsClosed(t *testing.T) {
	setConfigDir(t)
	previous := ancestorDispatch
	ancestorDispatch = func() (Record, error) {
		return Record{ID: "leaf", Status: "running", Route: route.Route{Provider: "databricks", Profile: "qa"}}, nil
	}
	t.Cleanup(func() { ancestorDispatch = previous })
	doc := withAncestorDispatch(proc.Values{route.OverlayVar: `{"provider":"anthropic"}`})[route.OverlayVar]
	var o agentconfig.Overlay
	if err := json.Unmarshal([]byte(doc), &o); err != nil || o.Fence == nil || len(o.Fence.Profiles) != 0 || o.Profile != "qa" {
		t.Fatalf("unregistered parent doc = %q, want qa under a fence admitting nothing", doc)
	}
}

// The ancestry walk decodes only records whose pid is in the chain and caches every pid in .children.idx.
func TestAncestryRecordsUseChildIndex(t *testing.T) {
	setConfigDir(t)
	mine := Record{ID: "mine", Agent: "claude", PID: os.Getpid(), Status: "done", Time: time.Now()}
	other := Record{ID: "other", Agent: "claude", PID: os.Getpid() + 1, Status: "running", Time: time.Now()}
	for _, rec := range []Record{mine, other} {
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
	}
	dir := paths.DispatchDir()
	for i := range 2 {
		if got := ancestorRecords(dir, map[int]int{mine.PID: 0}); len(got) != 1 || got[0].ID != mine.ID {
			t.Fatalf("ancestor records = %+v", got)
		}
		if i == 0 { // a same-stat garbage body: a second walk that decoded it would drop its cached pid
			garbleKeepingStat(t, filepath.Join(dir, other.ID+".json"))
		}
	}
	if idx := readChildIndex(dir); len(idx) != 2 || idx[other.ID].PID != other.PID {
		t.Fatalf("index = %+v", idx)
	}
}

func garbleKeepingStat(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("~", int(fi.Size()))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// storedRecord is a new record whose request carries a typed model, the auto workspace and a proxy endpoint.
func storedRecord(t *testing.T) Record {
	t.Helper()
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	req := RunOptions{Model: "claude-sonnet-5-5", ModelTyped: true, Fallback: route.FallbackAll,
		Route: route.Route{Provider: "databricks", Profile: route.AutoArm, Endpoint: "proxy"}}
	return NewRecord("claude", req, time.Now())
}

// A plain replay uses the stored overlay; a typed key overrides only itself with OriginFlag, leaving record untouched.
func TestReplayOverlayPlainAndTyped(t *testing.T) {
	rec := storedRecord(t)
	plain, prov, err := rec.ReplayOverlay(nil, agentconfig.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Model != "claude-sonnet-5-5" || plain.Fallback != route.FallbackAll || plain.Endpoint != "proxy" || plain.MaxDepth == nil {
		t.Fatalf("plain replay lost stored keys: %+v", plain)
	}
	if prov.Typed("model") || prov.Typed("unit") {
		t.Fatalf("plain replay marks typed keys: %v", prov)
	}
	typed, prov, err := rec.ReplayOverlay([]string{"model"}, agentconfig.Overlay{Model: "other-model"})
	if err != nil {
		t.Fatal(err)
	}
	if typed.Model != "other-model" || typed.Route() != plain.Route() || *typed.MaxDepth != *plain.MaxDepth || typed.Fallback != plain.Fallback {
		t.Fatalf("typed model changed more than the model: %+v vs %+v", typed, plain)
	}
	if prov.Of("model") != agentconfig.OriginFlag || prov.Of("maxDepth") != agentconfig.OriginRecord {
		t.Fatalf("origins = %v", prov)
	}
	*typed.MaxDepth = 99
	if again, _, _ := rec.ReplayOverlay(nil, agentconfig.Overlay{}); *again.MaxDepth == 99 {
		t.Fatal("a replay shares its limit pointer with the record")
	}
}

// The stored unit is read back exactly: the endpoint is neither cut nor rewritten between store and replay.
func TestReplayOverlayEndpointExact(t *testing.T) {
	rec := storedRecord(t)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got, _, err := back.ReplayOverlay(nil, agentconfig.Overlay{})
	if err != nil || got.Route() != (route.Route{Endpoint: "proxy", Provider: "databricks", Profile: route.AutoArm}) {
		t.Fatalf("replayed unit = %+v, err %v", got.Route(), err)
	}
}

// A name that fails validation is refused with its field named, never stored, and a replay refuses with it.
func TestStoredOverlayRefusesFreeText(t *testing.T) {
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	secret := "tok en@evil?x=1"
	rec := NewRecord("claude", RunOptions{Model: secret, ModelTyped: true}, time.Now())
	if rec.Overlay == nil || !strings.Contains(rec.Overlay.Refused, "model") || strings.Contains(rec.Overlay.Refused, secret) || rec.Overlay.Model != "" {
		t.Fatalf("overlay = %+v, want a refusal naming model without its value", rec.Overlay)
	}
	if _, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("replay err = %v, want a refusal naming model", err)
	}
	bad := StoredOverlay{Provider: "databricks", Profile: "no-such-row"}
	if err := bad.check(); err == nil || !strings.Contains(err.Error(), "profile") || strings.Contains(err.Error(), "no-such-row") {
		t.Fatalf("check = %v, want a refusal naming profile alone", err)
	}
	url := StoredOverlay{Provider: "databricks", Profile: route.AutoArm, Endpoint: "https://h.example/serving/tok"}
	if err := url.check(); err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("a URL no registry row holds must be refused: %v", err)
	}
}

// An old record with no stored overlay is built from its fields on read, limits included; an unknown key refuses.
func TestReplayOverlayOldRecordAndUnknownKey(t *testing.T) {
	setConfigDir(t)
	t.Setenv("HOME", t.TempDir())
	old := `{"schema":1,"id":"d1","agent":"claude","request":{"Model":"sonnet","model_typed":true,"route":{"provider":"databricks","profile":"auto"}},` +
		`"status":"ok","fallback_mode":"agents","max_depth":3,"max_children":4,"max_tree_tokens":500}`
	var rec Record
	if err := json.Unmarshal([]byte(old), &rec); err != nil {
		t.Fatal(err)
	}
	got, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{})
	if err != nil || rec.Overlay != nil {
		t.Fatalf("err %v, overlay %v", err, rec.Overlay)
	}
	if got.Model != "sonnet" || got.Fallback != "agents" || *got.MaxDepth != 3 || *got.MaxChildren != 4 || *got.MaxTreeTokens != 500 || got.Profile != route.AutoArm {
		t.Fatalf("old record overlay = %+v", got)
	}
	var newer Record
	if err := json.Unmarshal([]byte(`{"id":"d2","overlay":{"model":"m","restrict":"x"}}`), &newer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newer.ReplayOverlay(nil, agentconfig.Overlay{}); err == nil {
		t.Fatal("an unknown stored key must refuse a replay")
	}
}

func TestReplayOverlayFollowsAnsweringRoute(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	launch := route.Route{Provider: "databricks", Profile: "dev", Endpoint: "proxy"}
	final := route.Route{Provider: "databricks", Profile: "qa"}
	var old Record
	if err := json.Unmarshal([]byte(`{"request":{"Provider":"dev"},"provider":"qa","status":"error"}`), &old); err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]Record{
		"legacy provider field": old,
		"legacy":                {Request: RunOptions{Route: launch}, RanRoute: final},
		"fallback": {Request: RunOptions{Route: launch}, RanRoute: final,
			Overlay: &StoredOverlay{Provider: launch.Provider, Profile: launch.Profile, Endpoint: launch.Endpoint}},
		"hops": {Request: RunOptions{Route: launch}, Attempts: []wire.Hop{{Account: "databricks·qa", Profile: "qa"}, {NotStarted: true}},
			Overlay: &StoredOverlay{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{})
			if err != nil || got.Route() != final {
				t.Fatalf("replay = %+v, %v, want answering route %+v", got.Route(), err, final)
			}
			retry := NewRecord("codex", RunOptions{Route: got.Route()}, time.Now())
			retry.RetryOf = "original"
			again, _, err := retry.ReplayOverlay(nil, agentconfig.Overlay{})
			if err != nil || again.Route() != final {
				t.Fatalf("retry of retry = %+v, %v", again.Route(), err)
			}
		})
	}
}

func TestStoredOverlayRejectsUnregisteredNamesAndLimits(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	for name, ov := range map[string]StoredOverlay{
		"model":         {Model: "sk-alphanumeric-token"},
		"endpoint":      {Provider: "databricks", Profile: "dev", Endpoint: "sk-alphanumeric-token"},
		"profile":       {Provider: "github", Profile: "dev"},
		"negative cost": {BudgetUSD: new(-1.0)},
		"infinite cost": {BudgetUSD: new(math.Inf(1))},
		"nan cost":      {BudgetUSD: new(math.NaN())},
	} {
		if err := ov.check(); err == nil || strings.Contains(err.Error(), "sk-alphanumeric-token") {
			t.Errorf("%s: expected a refusal naming only the field, got %v", name, err)
		}
	}
	for _, ov := range []StoredOverlay{
		{Model: "sonnet"}, {Model: "gpt-6.1-sol"}, {Model: AutoModel},
		{Provider: "databricks", Profile: "dev", Endpoint: "proxy", BudgetUSD: new(0.0)},
	} {
		if err := ov.check(); err != nil {
			t.Errorf("registered name or typed limit refused: %v", err)
		}
	}
}

func TestRecordOmitsInheritedEnvValues(t *testing.T) {
	const inherited = `{"provider":"databricks","profile":"auto","set":{"ENV_NAME":"fake-sensitive-value"},` +
		`"tokenCommand":["echo","fake-inline-token"],"fence":{"profiles":["dev"]},"maxDepth":2}`
	rec := Record{Request: RunOptions{InheritedOverlay: inherited}}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fake-sensitive-value") || strings.Contains(string(raw), "fake-inline-token") {
		t.Fatal("record persisted inherited env or token-command values")
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	var ov agentconfig.Overlay
	if err := json.Unmarshal([]byte(back.Request.InheritedOverlay), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Provider != "databricks" || ov.Profile != "auto" || ov.Fence == nil || ov.Fence.Profiles[0] != "dev" || ov.MaxDepth == nil || *ov.MaxDepth != 2 {
		t.Fatal("safe inheritance lost its route, fence or depth")
	}
	if rec.Request.InheritedOverlay != inherited {
		t.Fatal("serializing the record mutated the live run's inherited values")
	}
}

func TestReplayOverlayKeepsExpiredScopeAndNativeLogin(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	cfg, _ := proxyteam.Load(nil)
	endpoint := cfg.Teams[0].Name + ":ended-scope"
	registeredURL := routeRegistry(t).Providers["dev"].BaseURL
	for name, tc := range map[string]struct {
		rec  Record
		want route.Route
	}{
		"expired scope": {Record{Overlay: &StoredOverlay{Provider: "databricks", Profile: "dev", Endpoint: endpoint}},
			route.Route{Provider: "databricks", Profile: "dev", Endpoint: endpoint}},
		"native login": {Record{Agent: "opencode", RanRoute: route.Route{Profile: "zen-free"},
			Overlay: &StoredOverlay{Provider: "databricks", Profile: "dev"}}, route.Route{}},
		"provider only": {Record{RanRoute: route.Route{Provider: "databricks"}, Overlay: &StoredOverlay{}},
			route.Route{Provider: "databricks"}},
		"partial observation": {Record{RanRoute: route.Route{Profile: "dev"},
			Overlay: &StoredOverlay{Provider: "databricks", Profile: "dev", Endpoint: "proxy", Identity: "sp"}},
			route.Route{Provider: "databricks", Profile: "dev", Endpoint: "proxy", Identity: "sp"}},
		"legacy endpoint path": {Record{Provider: "dev", Request: RunOptions{
			Route: route.Route{Provider: "databricks", Profile: "dev", Endpoint: registeredURL}}},
			route.Route{Provider: "databricks", Profile: "dev", Endpoint: registeredURL}},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, err := tc.rec.ReplayOverlay(nil, agentconfig.Overlay{})
			if err != nil || got.Route() != tc.want {
				t.Fatalf("replay = %+v, %v; want %+v", got.Route(), err, tc.want)
			}
		})
	}
}

// A stored [1m] marker on a registered model is a valid name: it is neither refused nor stripped on replay.
func TestStoredOverlayKeeps1MModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	for _, m := range []string{"sonnet[1m]", "opus[1M]", "claude-opus-4-6[1m]", "kimi-k3[1m]"} {
		rec := Record{Overlay: &StoredOverlay{Model: m}}
		if got, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{}); err != nil || got.Model != m {
			t.Errorf("%s: replay = %q, %v", m, got.Model, err)
		}
	}
	for _, m := range []string{"[1m]", "auto[1m]", "sonnet[1m][1m]", "sk-alphanumeric-token[1m]"} {
		if err := (StoredOverlay{Model: m}).check(); err == nil {
			t.Errorf("%s: an unregistered base passed behind a [1m] marker", m)
		}
	}
}

// An auto arm's endpoint and identity carry onto the row it picked; a different row's fallback still does not.
func TestReplayOverlayCarriesUnitAfterAutoPick(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	picked := route.Route{Provider: "databricks", Profile: "qa"}
	auto := &StoredOverlay{Provider: "databricks", Profile: route.AutoArm, Endpoint: "proxy", Identity: "sp"}
	for name, rec := range map[string]Record{
		"ran route": {RanRoute: picked, Overlay: auto},
		"hops":      {Attempts: []wire.Hop{{Account: "databricks·qa", Profile: "qa"}}, Overlay: auto},
		"legacy":    {RanRoute: picked, Request: RunOptions{Route: route.Route{Provider: "databricks", Profile: route.AutoArm, Endpoint: "proxy", Identity: "sp"}}},
	} {
		t.Run(name, func(t *testing.T) {
			want := route.Route{Provider: "databricks", Profile: "qa", Endpoint: "proxy", Identity: "sp"}
			if got, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{}); err != nil || got.Route() != want {
				t.Fatalf("replay = %+v, %v; want %+v", got.Route(), err, want)
			}
		})
	}
}

func TestReplayOverlayRejectsUnregisteredLegacyEndpoint(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeOverlay)
	rec := Record{Request: RunOptions{Route: route.Route{Provider: "databricks", Profile: "dev", Endpoint: "https://unregistered.example"}}}
	if _, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{}); err == nil {
		t.Fatal("legacy replay bypassed the registry endpoint check")
	}
}
