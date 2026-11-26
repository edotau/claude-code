package worktree

import (
	"bytes"
	"os"
	"path/filepath"
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

func readAttempts(t *testing.T) []LandAttempt {
	t.Helper()
	raw, err := os.ReadFile(LandAttemptsFile())
	if err != nil {
		t.Fatal(err)
	}
	return parseAttempts(raw)
}
