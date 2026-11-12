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

// A registry that fails to load is announced on stderr instead of silently dropping wire-model and hub handling.
func TestRouteChainWarnsWhenRegistryFailsToLoad(t *testing.T) {
	registryWarnOnce = sync.Once{} // the warning prints once per process; reset so -count>1 sees it again
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
