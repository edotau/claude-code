package cli

import (
	"context"
	"encoding/base64"
	"os/exec"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vendors"
)

// scratchRepo makes a throwaway git repo (no commits needed — router.IsWorkRepo only reads the origin
// remote) with origin set to url, so a test controls exactly what "the repo this leg runs in" looks like.
func scratchRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", origin)
	return dir
}

// setupWorkdirFixture isolates config state and makes opencode "configured" without a real login, so the
// sub-pick's gates turn on the work-repo rule alone, not on missing credentials.
func setupWorkdirFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "opencode", "exit 0\n")
	t.Setenv(vendors.OpencodeAPIKeyEnv, "test-key")
}

// TestResolveAskChainWorkRepoFromWorkdirNotCwd is the wiring's teeth: cwd is a personal repo (would pick
// the free Zen leg on its own) but --workdir names a work repo (its remote matches a configured
// workRepoMarkers entry) — the opencode sub-pick must see the work repo and skip straight to the paid
// opencode·go leg, never zen-free. workRepoMarkers defaults to none, so this test configures one.
func TestResolveAskChainWorkRepoFromWorkdirNotCwd(t *testing.T) {
	setupWorkdirFixture(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Opencode.WorkRepoMarkers = &[]string{"git.corp.example"}
		return nil
	}); err != nil {
		t.Fatalf("UpdateRouting() = %v", err)
	}
	personal := scratchRepo(t, "https://github.com/edotau/claude-code.git")
	work := scratchRepo(t, "https://git.corp.example/scm/x/y.git")
	t.Chdir(personal)

	_, chain, err := resolveAskChain(context.Background(), "opencode", "", "", "test prompt", agent.FallbackAgents, false, work)
	if err != nil {
		t.Fatalf("resolveAskChain() = %v", err)
	}
	if len(chain.Legs) == 0 {
		t.Fatal("no legs routed")
	}
	if chain.Legs[0].Billing == router.BillFree {
		t.Errorf("leg = %+v, want no free zen-free candidate: --workdir names a work repo even though cwd is personal", chain.Legs[0])
	}
	if chain.Legs[0].Account != "opencode·go" {
		t.Errorf("leg.Account = %q, want opencode·go (free blocked on the work repo)", chain.Legs[0].Account)
	}
}

// TestResolveAskChainWorkRepoDefaultsToCwdWhenWorkdirEmpty is the control: with no --workdir at all, the
// gate falls back to cwd — a personal repo cwd still gets the free Zen leg.
func TestResolveAskChainWorkRepoDefaultsToCwdWhenWorkdirEmpty(t *testing.T) {
	setupWorkdirFixture(t)
	personal := scratchRepo(t, "https://github.com/edotau/claude-code.git")
	t.Chdir(personal)

	_, chain, err := resolveAskChain(context.Background(), "opencode", "", "", "test prompt", agent.FallbackAgents, false, "")
	if err != nil {
		t.Fatalf("resolveAskChain() = %v", err)
	}
	if len(chain.Legs) == 0 || chain.Legs[0].Billing != router.BillFree {
		t.Errorf("leg = %+v, want the free zen-free candidate on a personal cwd with no --workdir override", chain.Legs[0])
	}
}
