package menu

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func seedDialog(t *testing.T, script string) (*dialog, *bytes.Buffer) {
	t.Helper()
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "") // menuTestEnv's claude pin folds into a record
	if got := vault.AgentRecords(agentpkg.RosterNames())["claude"].Profile; got != "" {
		t.Fatalf("claude record = %q, want a first-run start", got)
	}
	out := &bytes.Buffer{}
	return &dialog{reg: reg, out: out, br: bufio.NewReader(strings.NewReader(script))}, out
}

// seedIdx is agent's 1-based row number in the dialog.
func seedIdx(agent string) int {
	for i, a := range seedAgents() {
		if a == agent {
			return i + 1
		}
	}
	return 0
}

func TestSeedDismissWritesNothing(t *testing.T) {
	d, _ := seedDialog(t, "\n")
	if runSeed(d) {
		t.Fatal("dismissed dialog reported a write")
	}
	for a, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			t.Errorf("%s wrote profile %q on dismiss", a, r.Profile)
		}
	}
}

func TestSeedPickWritesProfilesOnly(t *testing.T) {
	d, out := seedDialog(t, "")
	// pick order in the picker: servable profiles, then the trailing off row
	offOf := func(a string) int { return len(seedProfiles(d, a)) + 1 }
	copilotProfiles := seedProfiles(d, "copilot")
	if len(copilotProfiles) == 0 {
		t.Skip("registry serves copilot with no profile")
	}
	want := copilotProfiles[0]
	script := strings.Join([]string{
		strconv.Itoa(seedIdx("copilot")), "1",
		strconv.Itoa(seedIdx("codex")), strconv.Itoa(offOf("codex")),
		strconv.Itoa(len(seedAgents()) + 1),
	}, "\n") + "\n"
	d.br = bufio.NewReader(strings.NewReader(script))
	if !runSeed(d) {
		t.Fatalf("save reported no write:\n%s", out)
	}
	recs := vault.AgentRecords(agentpkg.RosterNames())
	if recs["copilot"].Profile != want || recs["codex"].Profile != "" {
		t.Errorf("records = copilot %q codex %q, want %q and off", recs["copilot"].Profile, recs["codex"].Profile, want)
	}
	// reopening shows the stored value
	d2, out2 := seedDialog(t, "\n")
	runSeed(d2)
	if !strings.Contains(out2.String(), want) {
		t.Errorf("reopened dialog does not show %q:\n%s", want, out2)
	}
}

func TestSeedFirstRunNonInteractivePointsAtRow(t *testing.T) {
	d, out := seedDialog(t, "")
	seedOnFirstRun(d, "")
	if !strings.Contains(out.String(), "seed agents") {
		t.Errorf("no pointer line: %q", out)
	}
	for _, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			t.Fatalf("non-interactive first run wrote %+v", r)
		}
	}
}

func TestSeedRowFitsEightyColumns(t *testing.T) {
	menuTestEnv(t)
	laid, _ := RowsOf(settingsRows(&dialog{}))
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
}

// Given a remembered last-used profile, seeding a registered profile after sign-in forgets it and sets the record.
func TestSeedFromSignInForgetsLastProfileAndSetsRecord(t *testing.T) {
	d, out := seedDialog(t, "")
	profiles := seedProfiles(d, "copilot")
	if len(profiles) == 0 {
		t.Skip("registry serves copilot with no profile")
	}
	agentpkg.RecordLastProfile("copilot", "stale-profile", nil)
	lastProfile := func() string {
		raw, _ := os.ReadFile(filepath.Join(paths.StateDir(), "last-profile.json"))
		return string(raw)
	}
	if !strings.Contains(lastProfile(), "stale-profile") {
		t.Fatalf("setup: last-profile.json lacks the entry: %s", lastProfile())
	}
	seedFromSignIn(d, "copilot", profiles[0])
	if strings.Contains(lastProfile(), "stale-profile") {
		t.Errorf("last-used profile survived sign-in: %s", lastProfile())
	}
	if got := vault.AgentRecords(agentpkg.RosterNames())["copilot"].Profile; got != profiles[0] || !d.changed {
		t.Errorf("copilot profile = %q (changed=%v), want %q; dialog said %q", got, d.changed, profiles[0], out)
	}
}

// Given one invalid profile among the choices, saveSeed writes no agent's profile.
func TestSaveSeedInvalidProfileWritesNothing(t *testing.T) {
	d, _ := seedDialog(t, "")
	if saveSeed(d, map[string]string{"claude": "", "codex": "no-such-profile"}) {
		t.Fatal("saveSeed reported success for an unknown profile")
	}
	for a, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			t.Errorf("%s persisted profile %q after a failed save", a, r.Profile)
		}
	}
}
