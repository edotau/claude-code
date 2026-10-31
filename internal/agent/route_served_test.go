package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func dbxProv(name string) *providers.Provider {
	return &providers.Provider{Name: name, Preset: providers.PresetDatabricks, Host: "https://" + name + ".example.com",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "DBX_" + strings.ToUpper(name) + "_PAT"}}
}

// listing is a fresh served listing: ids mapped to their ready state, in a stable order.
func listing(profile string, ready map[string]string) Served {
	s := Served{Profile: profile, FetchedAt: time.Now()}
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range ready {
			if !yield(k) {
				return
			}
		}
	}) {
		s.Models = append(s.Models, ServedModel{ID: id, Ready: ready[id]})
	}
	return s
}

// stubServed makes the given profiles' listings fresh; any other profile has no cache. It also seeds a hermetic vault.
func stubServed(t *testing.T, menu []providers.OpencodeModel, byProfile ...Served) {
	t.Helper()
	vaulttest.Use(t)
	if menu != nil {
		if err := providers.SetOpencodeMenu(menu); err != nil {
			t.Fatal(err)
		}
	}
	orig := servedListing
	servedListing = func(profile string, _ time.Time) (Served, bool) {
		for _, s := range byProfile {
			if s.Profile == profile {
				return s, true
			}
		}
		return Served{}, false
	}
	t.Cleanup(func() { servedListing = orig })
}

var (
	respA = providers.OpencodeModel{ID: "m-a", Label: "A", Responses: true}
	respB = providers.OpencodeModel{ID: "m-b", Label: "B", Responses: true}
	respC = providers.OpencodeModel{ID: "m-c", Label: "C", Responses: true}
	chatX = providers.OpencodeModel{ID: "m-x", Label: "X"}
)

// switch off (the default): a fresh cache that would change the pick leaves the static first Responses model.
func TestServedSwitchOffKeepsStaticPick(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	t.Setenv("CLAUDE_PROFILE", "dev")
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{respA, respB}); err != nil {
		t.Fatal(err)
	}
	if err := SaveServed(listing("dev", map[string]string{"m-a": "NOT_READY", "m-b": "READY"})); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, model, ok := DatabricksResponses(reg, time.Now()); !ok || model != "m-a" {
		t.Fatalf("switch off picked %q (ok=%v), want the static m-a", model, ok)
	}
	on := strings.Replace(routeDbxRow, `{"providers"`, `{"routing":{"servedAware":true},"providers"`, 1)
	vaulttest.Put(t, vault.Overlay, "providers", on)
	reg, err = providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, model, ok := DatabricksResponses(reg, time.Now()); !ok || model != "m-b" {
		t.Fatalf("switch on picked %q (ok=%v), want the served m-b", model, ok)
	}
}

// codex's served pick skips an unlisted entry and a listed NOT_READY one; any-agent scan keeps the non-Responses entry.
func TestServedFirstSkipsUnlistedAndNotReady(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, []providers.OpencodeModel{chatX, respA, respB, respC},
		listing("dev", map[string]string{"m-x": "READY", "m-b": "NOT_READY", "m-c": "READY"}))
	now := time.Now()
	if got, fresh := ServedFirst(p, true, now); got != "m-c" || !fresh {
		t.Errorf("codex pick = %q fresh=%v, want m-c (m-a unlisted, m-b NOT_READY)", got, fresh)
	}
	if got, _ := ServedFirst(p, false, now); got != "m-x" {
		t.Errorf("any-agent pick = %q, want the first READY entry m-x", got)
	}
}

// an absent cache is not fresh (callers stay static); a fresh listing with nothing served is fresh and empty.
func TestServedAbsentStaysStaticFreshEmptyServesNothing(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, []providers.OpencodeModel{respA})
	if got, fresh := ServedFirst(p, true, time.Now()); got != "" || fresh {
		t.Errorf("absent cache: ServedFirst = %q fresh=%v, want static (not fresh)", got, fresh)
	}
	if ready, fresh := ServedReady(p, "m-a", time.Now()); ready || fresh {
		t.Errorf("absent cache: ServedReady = ready %v fresh %v, want false false", ready, fresh)
	}
	stubServed(t, []providers.OpencodeModel{respA}, listing("dev", nil))
	if got, fresh := ServedFirst(p, true, time.Now()); got != "" || !fresh {
		t.Errorf("fresh empty: ServedFirst = %q fresh=%v, want empty and fresh", got, fresh)
	}
}

// fresh-empty listing: DatabricksResponses is not ok and codex gets no Databricks candidate.
func TestServedFreshEmptyDropsCodexDatabricksCandidate(t *testing.T) {
	stubServed(t, nil, listing("dev", nil))
	accts, _ := dbxCandidates(t, "dev", []providers.OpencodeModel{respA}, nil)
	if slices.Contains(accts, "databricks·dev") {
		t.Fatalf("codex accounts = %v, want no databricks·dev when nothing is served", accts)
	}
}

// each profile reads its own cache: dev serves m-b, qa serves m-c.
func TestServedPerProfileIsolation(t *testing.T) {
	stubServed(t, []providers.OpencodeModel{respA, respB, respC},
		listing("dev", map[string]string{"m-b": "READY"}), listing("qa", map[string]string{"m-c": "READY"}))
	now := time.Now()
	if got, _ := ServedFirst(dbxProv("dev"), true, now); got != "m-b" {
		t.Errorf("dev pick = %q, want m-b", got)
	}
	if got, _ := ServedFirst(dbxProv("qa"), true, now); got != "m-c" {
		t.Errorf("qa pick = %q, want m-c", got)
	}
	if got, fresh := ServedFirst(dbxProv("other"), true, now); got != "" || fresh {
		t.Errorf("uncached profile = %q fresh=%v, want static", got, fresh)
	}
}

// a pin the fresh listing lacks is swapped: copilot takes any READY menu entry, codex only a Responses one.
func TestPinnedModelSwapsUnservedPin(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, []providers.OpencodeModel{chatX, respA},
		listing("dev", map[string]string{"m-x": "READY", "m-a": "READY"}))
	cell := Cell{Model: "m-pin", Account: "databricks·dev"}
	if got := pinnedModel("copilot", "sonnet", cell, p, "m-pin", time.Now()); got != "m-x" {
		t.Errorf("copilot swap = %q, want m-x (any READY)", got)
	}
	if got := pinnedModel("codex", "sonnet", cell, p, "m-pin", time.Now()); got != "m-a" {
		t.Errorf("codex swap = %q, want m-a (Responses only)", got)
	}
	if got := pinnedModel("codex", "sonnet", cell, p, "m-a", time.Now()); got != "m-a" {
		t.Errorf("a READY pin = %q, want it kept", got)
	}
}

// the swap Skip names an explicit pin only: a tier-slot swap and a byok request add none.
func TestGateProfileSwapSkipOnlyForExplicitPin(t *testing.T) {
	tb := testTable(t) // first: it resets CLAUDE_CONFIG_DIR, which would orphan the vault below
	stubServed(t, []providers.OpencodeModel{respA}, listing("dev", map[string]string{"m-a": "READY"}))
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	skips := func(pin string) bool {
		res := &PickResult{}
		tb.gateProfile(context.Background(), reg, "sonnet", "codex", "dev", pin, "", router.State{}, PickOpts{}, time.Now(), allConfigured, res)
		return slices.ContainsFunc(res.Skipped, func(s Skip) bool { return strings.Contains(s.Reason, "is not READY on dev") })
	}
	if !skips("m-pin") {
		t.Error("an explicit unserved pin must surface a swap Skip")
	}
	if skips("") || skips(BYOKModel) {
		t.Error("an empty or byok pin must not surface a swap Skip")
	}
}

// pickPool drops a pool entry the fresh listing marks not READY.
func TestPickPoolDropsFreshNotReady(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, nil, listing("dev", map[string]string{"m-a": "NOT_READY", "m-b": "READY"}))
	got, err := pickPool("copilot", p, []string{"m-a", "m-b"}, time.Now())
	if err != nil || got != "m-b" {
		t.Fatalf("pickPool = %q, %v, want m-b", got, err)
	}
}

// a live MarkUnserved makes ServedReady false and ServedFirst skip the model.
func TestMarkUnservedHidesModel(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, []providers.OpencodeModel{respA, respB}, listing("dev", map[string]string{"m-a": "READY", "m-b": "READY"}))
	if err := MarkUnserved(p, "m-a"); err != nil {
		t.Fatal(err)
	}
	if ready, fresh := ServedReady(p, "m-a", time.Now()); ready || !fresh {
		t.Errorf("marked m-a: ready=%v fresh=%v, want not ready", ready, fresh)
	}
	if got, _ := ServedFirst(p, true, time.Now()); got != "m-b" {
		t.Errorf("ServedFirst = %q, want m-b past the marked m-a", got)
	}
}

// a mark older than an hour is ignored, and the next write drops it while keeping a younger one.
func TestMarkUnservedExpiresAfterAnHour(t *testing.T) {
	p := dbxProv("dev")
	stubServed(t, []providers.OpencodeModel{respA}, listing("dev", map[string]string{"m-a": "READY", "m-b": "READY"}))
	acct := router.ProviderAccount(p.Name)
	now := time.Now()
	if err := router.Update(func(s router.State) {
		e := s[acct]
		e.Unserved = map[string]time.Time{"m-a": now.Add(-2 * time.Hour), "m-old": now.Add(-3 * time.Hour), "m-new": now.Add(-10 * time.Minute)}
		s[acct] = e
	}); err != nil {
		t.Fatal(err)
	}
	if ready, _ := ServedReady(p, "m-a", now); !ready {
		t.Error("a 2h-old mark must be ignored")
	}
	if err := MarkUnserved(p, "m-b"); err != nil {
		t.Fatal(err)
	}
	got := router.LoadState()[acct].Unserved
	if _, ok := got["m-old"]; ok || got["m-new"].IsZero() || got["m-b"].IsZero() {
		t.Errorf("after write Unserved = %v, want m-old pruned, m-new and m-b kept", got)
	}
}
