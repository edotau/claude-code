package worktree

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

// A commit on main during the gate re-checks instead of failing, and both attempts are recorded.
func TestPromoteRevalidates(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	flag := filepath.Join(t.TempDir(), "moved")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify = false
	req.Verify = "[ -e " + textutil.ShellQuote(flag) + " ] || { : > " + textutil.ShellQuote(flag) +
		"; git -C " + textutil.ShellQuote(main) + " commit -q --allow-empty -m moved; }"
	if _, err := Land(t.Context(), repo, req); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	requireContains(t, "output", out.String(), "rebase:     skipped", "rebased:", "re-check:", "landed a:")
	var outcomes []Outcome
	for _, a := range readAttempts(t) {
		outcomes = append(outcomes, a.Outcome)
	}
	if len(outcomes) != 2 || outcomes[0] != OutcomeTargetMoved || outcomes[1] != OutcomeLanded {
		t.Fatalf("attempt outcomes = %v, want [target_moved landed]", outcomes)
	}
}

// A re-check that fails after the target moved stops the land: nothing reaches main.
func TestPromoteFailedRecheckNeverLands(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	flag := filepath.Join(t.TempDir(), "moved")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify = false
	req.Verify = "if [ -e " + textutil.ShellQuote(flag) + " ]; then printf 'FAIL\\tex/z\\t0.1s\\n'; exit 1; fi; : > " + textutil.ShellQuote(flag) +
		"; git -C " + textutil.ShellQuote(main) + " commit -q --allow-empty -m moved"
	if _, err := Land(t.Context(), repo, req); err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("Land = %v, want verify failed\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(main, "lane.txt")); err == nil {
		t.Fatalf("a failed re-check still landed the lane\n%s", out.String())
	}
	if got := readAttempts(t); len(got) == 0 || strings.Join(got[len(got)-1].FailedPkgs, ",") != "ex/z" {
		t.Fatalf("last attempt must name the failed package ex/z: %+v", got)
	}
	if got, _ := ListFailures(FailuresDir()); len(got) != 1 || got[0].Kind != FailLaneGate {
		t.Fatalf("failed gate must broadcast one lane_gate failure: %+v", got)
	}
}

func readAttempts(t *testing.T) []LandAttempt {
	t.Helper()
	raw, err := os.ReadFile(LandAttemptsFile())
	if err != nil {
		t.Fatal(err)
	}
	return parseAttempts(raw)
}

// A gate that is not a Go package gate names its command and exit code in the broadcast summary.
func TestGateFailTextNamesCommandAndExit(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 3").Run()
	if got := gateFailText("false", err); got != "verify `false` exited 3" {
		t.Fatalf("gateFailText = %q", got)
	}
}
