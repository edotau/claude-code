package cli

import (
	"context"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
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
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	t.Setenv(agent.OpencodeAPIKeyEnv, "test-key")
}

// A work --workdir must skip free Zen even when the process cwd is a personal repo.
func TestClientPrepareWorkRepoFromWorkdirNotCwd(t *testing.T) {
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

	res := runPreparedWorkdir(t, work)
	if res.Hops[0].Account != "opencode·go" {
		t.Errorf("leg.Account = %q, want opencode·go (free blocked on the work repo)", res.Hops[0].Account)
	}
}

// Without --workdir, a personal process cwd still gets the free Zen leg.
func TestClientPrepareWorkRepoDefaultsToCwdWhenWorkdirEmpty(t *testing.T) {
	setupWorkdirFixture(t)
	personal := scratchRepo(t, "https://github.com/edotau/claude-code.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, "")
	if !strings.Contains(res.Hops[0].Account, "zen-free") {
		t.Errorf("leg = %+v, want free Zen on a personal cwd with no --workdir override", res.Hops[0])
	}
}

func runPreparedWorkdir(t *testing.T, workdir string) agent.Result {
	t.Helper()
	client := newClient()
	p, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "test prompt",
		Fallback: agent.FallbackAgents, WorkDir: workdir}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	res, err := client.Exec(context.Background(), p, nil, agent.DispatchOpts{})
	if err != nil || len(res.Hops) != 1 || res.Answer != "ok" {
		t.Fatalf("Exec() = %+v, %v; want one successful leg", res, err)
	}
	if res.DispatchID != p.Record.ID || res.DispatchID == "" {
		t.Fatalf("dispatch id = %q, want %q", res.DispatchID, p.Record.ID)
	}
	return res
}
