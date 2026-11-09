package agent

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A candidate dropped after the pick must not lend its slot to the next survivor of the same agent and account.
func TestPreviewOutcomeMatchesByRungNotPosition(t *testing.T) {
	cell := func(m string) router.Cell { return router.Cell{Model: m} }
	pick := router.PickResult{Candidates: []router.Candidate{
		{Agent: "codex", Cell: cell("m1"), Profile: "A"},
		{Agent: "codex", Cell: cell("m2"), Profile: "B"},
	}}
	chain := Chain{Dispatches: []Dispatch{{Agent: &pinFallbackRunner{name: "codex"}, Model: "m2", rung: 2}}}
	got := previewOutcome(chain, pick).Candidates
	if len(got) != 1 || got[0].Profile != "B" || got[0].Model != "m2" {
		t.Fatalf("candidates = %+v, want only rung 2 (profile B, model m2)", got)
	}
}

// A registry that fails to load is announced on stderr instead of silently dropping wire-model and hub handling.
func TestRouteChainWarnsWhenRegistryFailsToLoad(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	_, _, rerr := RouteChainFor(context.Background(), "claude", "", "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured})
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(out), "provider registry failed to load") {
		t.Fatalf("stderr = %q, want the registry-load warning", out)
	}
}

// Every dispatch the router builds carries the 1-based rung of the pick candidate it came from.
func TestRouteChainStampsRungOnEveryDispatch(t *testing.T) {
	vaulttest.Use(t)
	chain, pick, err := RouteChainFor(context.Background(), "claude", "", "sonnet", router.FallbackAll, false,
		router.PickOpts{Configured: allConfigured})
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Dispatches) == 0 {
		t.Fatal("no dispatches routed")
	}
	for _, d := range chain.Dispatches {
		if d.rung < 1 || d.rung > len(pick.Candidates) {
			t.Fatalf("dispatch %s rung = %d, want 1..%d", d.Agent.Name(), d.rung, len(pick.Candidates))
		}
	}
	if got := previewOutcome(chain, pick).Candidates; len(got) != len(chain.Dispatches) {
		t.Fatalf("preview candidates = %d, want %d", len(got), len(chain.Dispatches))
	}
}

// A preview leads only with an explicit lead; the session profile alone adds none (callers behave as before).
func TestPreviewRouteLeadsOnlyWhenAsked(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	t.Setenv("CLAUDE_PROFILE", "no-such-row")
	for _, lead := range []string{"", "no-such-row"} {
		q := RouteReq{Agent: "codex", Task: "x", Lead: lead, Fallback: router.FallbackAll, Preview: true}
		_, res, err := Resolve(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var skips []string
		for _, s := range res.Skipped {
			skips = append(skips, s.Reason)
		}
		if got := strings.Contains(strings.Join(skips, "\n"), "no-such-row"); got != (lead != "") {
			t.Fatalf("lead %q: skip naming no-such-row = %v, skipped %+v", lead, got, res.Skipped)
		}
	}
}

// A broken registry warns once per process, not on every route or preview.
func TestRegistryLoadWarningPrintsOnce(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	registryWarnOnce = sync.Once{}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	loadRegistryWarn()
	loadRegistryWarn()
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	if n := strings.Count(string(out), "provider registry failed to load"); n != 1 {
		t.Fatalf("warning printed %d times, want 1: %q", n, out)
	}
}
