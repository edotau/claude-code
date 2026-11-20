package ask

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Given a background spawn that fails (the test binary is never relaunched), Then the saved record leaves running.
func TestAskSpawnFailureFinishesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	if code := askSpawn(t.Context(), flag.NewFlagSet("ask", flag.ContinueOnError), &agent.Prepared{Record: rec}, false, helptext.Text, "claude"); code != 1 {
		t.Fatalf("askSpawn = %d, want 1", code)
	}
	got, err := agent.LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error == "" {
		t.Errorf("record = %q/%q, want error with the cause", got.Status, got.Error)
	}
}

// --retry of a databricks:dev run re-arms dev with no --overlay; a typed --profile replaces the recorded route whole.
func TestCmdAskRetryReArmsRecordedConfinement(t *testing.T) {
	dump := armChildEnvFixture(t)
	runCLI(t, "ask", "--agent", "codex", "--overlay", `{"provider":"databricks","profile":"dev"}`, "--fallback", "none", "go")
	recs := agent.RecentRecords(1)
	if len(recs) != 1 {
		t.Fatalf("armed run saved %d records, want 1", len(recs))
	}
	unarm := func() { // the armed run exported its confinement into this process and cooled dev on its 402
		if err := os.Remove(router.StatePath()); err != nil {
			t.Fatal(err)
		}
		t.Setenv(providers.EnvSessionProfile, "qa")
		t.Setenv(route.OverlayVar, "")
	}
	unarm()
	code, _, stderr := runCLI(t, "ask", "--retry", recs[0].ID)
	bins, envs := armDumps(t, dump)
	if code == 0 || !slices.Equal(bins, []string{"codex", "codex"}) {
		t.Fatalf("retry exit %d launches %v, want one more codex dev hop\n%s", code, bins, stderr)
	}
	assertArmDevHop(t, "codex", "linux", envs[1])
	unarm()
	code, _, stderr = runCLI(t, "ask", "--retry", recs[0].ID, "--profile", "chatgpt")
	if code == 2 || strings.Contains(stderr, "outside overlay") { // a typed route replaces the record's whole: no fence from it
		t.Fatalf("retry --profile chatgpt: exit %d stderr %q, want the typed route to run, not the recorded fence", code, stderr)
	}
}
