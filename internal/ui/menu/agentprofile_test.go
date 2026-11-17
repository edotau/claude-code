package menu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runProfile drives agent's Profile row picker with answer and returns the dialog output.
func runProfile(t *testing.T, reg *providers.Registry, agent, answer string) string {
	t.Helper()
	in := strings.NewReader(answer)
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	agentPinRows(d, agent)[0].run()
	return out.String()
}

// The merged Profile row lists off first then the own login; picking writes the record's Profile and keeps its model.
func TestProfileRowPicksOffAndOwnLogin(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	vaulttest.SetRecord(t, "codex", "model", "keep-me")
	out := runProfile(t, reg, "codex", "2\n")
	if !strings.Contains(out, "1) off") || !strings.Contains(out, "2) chatgpt") {
		t.Errorf("picker = %q, want off then the own login chatgpt", out)
	}
	if r := vault.GetAgentRecord("codex"); r.Profile != "chatgpt" || r.Model != "keep-me" {
		t.Errorf("record after own-login pick = %+v, want profile chatgpt, model kept", r)
	}
	runProfile(t, reg, "codex", "1\n")
	if r := vault.GetAgentRecord("codex"); r.Profile != "" || r.Model != "keep-me" {
		t.Errorf("record after off = %+v, want profile off, model kept", r)
	}
}

// opencode's menu lists its own login zen-free right after off, which the old Route row omitted.
func TestProfileRowOpencodeListsZenFree(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	out := runProfile(t, reg, "opencode", "2\n")
	if !strings.Contains(out, "2) "+opencode.OwnLogin) {
		t.Errorf("picker = %q, want zen-free as row 2", out)
	}
	if got := vault.GetAgentRecord("opencode").Profile; got != opencode.OwnLogin {
		t.Errorf("profile = %q, want %q", got, opencode.OwnLogin)
	}
}

// Every agent's Profile row is one line (its page width: TestMenuRowWidthsFitEightyColumns), claude's automatic.
func TestProfileRowIsOneLineClaudeAutomatic(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	d := &dialog{reg: reg, out: &bytes.Buffer{}}
	for _, a := range seedAgents() {
		if r := agentPinRows(d, a)[0]; r.label != "Pin / default" || strings.Contains(r.desc, "\n") {
			t.Errorf("%s profile row = %q %q, want one Profile line", a, r.label, r.desc)
		}
	}
	if desc := agentPinRows(d, "claude")[0].desc; !strings.Contains(desc, "automatic") {
		t.Errorf("claude row = %q, want automatic", desc)
	}
}

// A sign-in seeds only an EMPTY profile with the own login; a chosen profile is never overridden.
func TestAgentActiveOnSeedsOnlyEmptyProfile(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	d := &dialog{reg: reg, out: &bytes.Buffer{}}
	agentActiveOn(d, "codex")
	if got := vault.GetAgentRecord("codex").Profile; got != "chatgpt" {
		t.Fatalf("off codex after sign-in = %q, want chatgpt", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	agentActiveOn(d, "codex")
	if got := vault.GetAgentRecord("codex").Profile; got != "openai" {
		t.Errorf("chosen profile overridden: %q, want openai", got)
	}
}

func seedDialog(t *testing.T, script string) (*dialog, *bytes.Buffer) {
	t.Helper()
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
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
	runSeed(d)
	for a, r := range vault.AgentRecords(agentpkg.RosterNames()) {
		if r.Profile != "" {
			t.Errorf("%s wrote profile %q on dismiss", a, r.Profile)
		}
	}
}

func TestSeedPickWritesProfilesOnly(t *testing.T) {
	d, _ := seedDialog(t, "")
	// pick order in the picker: the off row (1), then servable profiles
	copilotProfiles := configure.SeedProfiles(d.reg, "copilot")
	if len(copilotProfiles) == 0 {
		t.Skip("registry serves copilot with no profile")
	}
	want := copilotProfiles[0]
	script := strings.Join([]string{
		strconv.Itoa(seedIdx("copilot")), "2",
		strconv.Itoa(seedIdx("codex")), "1",
		strconv.Itoa(len(seedAgents()) + 1),
	}, "\n") + "\n"
	d.br = bufio.NewReader(strings.NewReader(script))
	runSeed(d)
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

// Given a registered profile, seeding after sign-in sets the agent record and marks the dialog changed.
func TestSeedFromSignInSetsRecord(t *testing.T) {
	d, out := seedDialog(t, "")
	profiles := configure.SeedProfiles(d.reg, "copilot")
	if len(profiles) == 0 {
		t.Skip("registry serves copilot with no profile")
	}
	if got := signInProfile(d, "copilot"); got != profiles[0] {
		t.Skipf("copilot sign-in profile = %q, not %q", got, profiles[0])
	}
	agentActiveOn(d, "copilot")
	if got := vault.AgentRecords(agentpkg.RosterNames())["copilot"].Profile; got != profiles[0] || !d.changed {
		t.Errorf("copilot profile = %q (changed=%v), want %q; dialog said %q", got, d.changed, profiles[0], out)
	}
}

func TestPinCopilotProfileEmptyRefusedKeepsPin(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "copilot", "profile", "copilot")
	vaulttest.SetRecord(t, "copilot", "model", "keep-model")
	d := &dialog{reg: reg, out: &bytes.Buffer{}}
	if pinCopilotProfile(d, "") {
		t.Fatalf("empty pin must be refused: %s", d.out)
	}
	if got := vault.GetAgentRecord("copilot"); got.Profile != "copilot" || got.Model != "keep-model" {
		t.Fatalf("refused pin must keep the record: %+v", got)
	}
}

func TestSaveSeedMarksDialogChanged(t *testing.T) {
	d, out := seedDialog(t, "")
	if !saveSeed(d, map[string]string{"codex": "chatgpt"}) {
		t.Fatalf("saveSeed failed: %s", out)
	}
	if got := vault.GetAgentRecord("codex").Profile; got != "chatgpt" {
		t.Fatalf("codex profile = %q, want chatgpt", got)
	}
	if !d.changed {
		t.Fatal("saved seed profiles must trigger config regeneration")
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

// pickWorkspace drives agent's Databricks row with answer; the sign-in check mints through mint, never the network.
func pickWorkspace(t *testing.T, d *dialog, agent, answer string, mint func(context.Context, string) (databricks.MintResult, string, error)) (*billingRow, string) {
	t.Helper()
	orig := dbxMint
	dbxMint = mint
	t.Cleanup(func() { dbxMint = orig })
	in := strings.NewReader(answer)
	var out bytes.Buffer
	d.o.In, d.br, d.out = in, bufio.NewReader(in), &out
	row := dbxWorkspaceRow(d, agent)
	if row == nil {
		t.Fatalf("%s has no Databricks row", agent)
	}
	row.run()
	return row, out.String()
}

func mintM2M(context.Context, string) (databricks.MintResult, string, error) {
	return databricks.MintResult{AuthType: providers.AuthM2M}, "fixture-token", nil
}

// workspaceFixture pins opencode on qa, codex/gemini/claude on their own logins, copilot unpinned.
func workspaceFixture(t *testing.T, d *dialog) {
	t.Helper()
	vaulttest.SetRecord(t, "opencode", "profile", "qa")
	for _, a := range []string{"codex", "gemini", "claude"} {
		vaulttest.SetRecord(t, a, "profile", signInProfile(d, a))
	}
	vaulttest.SetRecord(t, "copilot", "profile", "")
}

func profiles(agents ...string) map[string]string {
	m := map[string]string{}
	for _, a := range agents {
		m[a] = vault.GetAgentRecord(a).Profile
	}
	return m
}

// Picking a workspace moves the page's agent and every agent on another workspace, no one else.
func TestDatabricksPickSyncsAgentsOnWorkspaces(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	workspaceFixture(t, d)
	before := profiles("claude", "gemini", "copilot")
	_, out := pickWorkspace(t, d, "codex", "2\n", mintM2M)
	if got := vault.GetAgentRecord("codex").Profile; got != "prod" {
		t.Errorf("page agent codex on own login: profile = %q, want prod", got)
	}
	if got := vault.GetAgentRecord("opencode").Profile; got != "prod" {
		t.Errorf("opencode on qa: profile = %q, want prod", got)
	}
	if got := profiles("claude", "gemini", "copilot"); !reflect.DeepEqual(got, before) {
		t.Errorf("own-login and unpinned agents moved: got %v, want %v", got, before)
	}
	if !strings.Contains(out, "databricks prod: also moved opencode\n") || strings.Count(out, "token ok") != 1 {
		t.Errorf("output = %q, want one moved line naming opencode and one token check", out)
	}
}

// The Pin / default picker syncs a workspace pick like the Databricks row and keeps other profiles per-agent.
func TestPinPickerSyncsOnlyWorkspacePicks(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	for _, tc := range []struct{ pick, opencode string }{{"prod", "prod"}, {"chatgpt", "qa"}} {
		workspaceFixture(t, d)
		idx := slices.Index(agentProfileChoices(d, "codex"), tc.pick)
		if idx < 0 {
			t.Fatalf("codex picker lacks %s", tc.pick)
		}
		in := strings.NewReader(strconv.Itoa(idx+1) + "\n")
		var out bytes.Buffer
		d.o.In, d.br, d.out = in, bufio.NewReader(in), &out
		if _, err := pickAgentProfile(d, "codex", ""); err != nil {
			t.Fatal(err)
		}
		if got := vault.GetAgentRecord("codex").Profile; got != tc.pick {
			t.Errorf("pick %s: codex = %q", tc.pick, got)
		}
		if got := vault.GetAgentRecord("opencode").Profile; got != tc.opencode {
			t.Errorf("pick %s: opencode = %q, want %s", tc.pick, got, tc.opencode)
		}
	}
}

// A panic in the token mint prints one generic line, never the panic value.
func TestDatabricksTokenCheckPanicPrintsGenericLine(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	boom := func(context.Context, string) (databricks.MintResult, string, error) { panic("secret-panic-value") }
	_, out := pickWorkspace(t, d, "codex", "1\n", boom)
	if strings.Contains(out, "secret-panic-value") || !strings.Contains(out, "databricks qa: token check failed unexpectedly") {
		t.Errorf("output = %q, want the generic failure line only", out)
	}
}

// A failed token check reports the fix and leaves the pin in place.
func TestDatabricksPinSurvivesFailedTokenCheck(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	fail := func(context.Context, string) (databricks.MintResult, string, error) {
		return databricks.MintResult{}, "", errors.New("invalid_client\nbad secret")
	}
	_, out := pickWorkspace(t, d, "codex", "1\n", fail)
	if got := vault.GetAgentRecord("codex").Profile; got != "qa" {
		t.Errorf("profile = %q after a failed check, want qa kept", got)
	}
	if !strings.Contains(out, "databricks qa: invalid_client bad secret · fix: "+providers.LoginCommand("qa")) {
		t.Errorf("output = %q, want the short error and the fix hint", out)
	}
}

// No credentialed workspace, no row.
func TestDatabricksRowAbsentWithoutCredentialedWorkspace(t *testing.T) {
	reg := menuTestEnv(t)
	for _, a := range setupAgentNames {
		if r := dbxWorkspaceRow(&dialog{reg: reg, out: discardWriter{}}, a); r != nil {
			t.Errorf("%s Databricks row = %+v with no signed-in workspace", a, r)
		}
	}
}

func TestOwnSignInReplacesWorkspacePin(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	for _, a := range setupAgentNames {
		vaulttest.SetRecord(t, a, "profile", "qa")
		rows := []billingRow{{label: "Login", run: func() bool { return false }}}
		activateOnSignIn(d, a, rows)
		if rows[0].run() || vault.GetAgentRecord(a).Profile != "qa" {
			t.Fatalf("%s failed sign-in changed the workspace pin", a)
		}
		rows = []billingRow{{label: "Login", run: func() bool { return true }}}
		activateOnSignIn(d, a, rows)
		if !rows[0].run() {
			t.Fatalf("%s own login failed", a)
		}
		want := signInProfile(d, a)
		if got := vault.GetAgentRecord(a).Profile; got != want || got == "qa" || got == "" {
			t.Errorf("%s profile = %q, want own login %q", a, got, want)
		}
	}
}

func TestFallbackRowShowsAutomaticCopilotAndExplicitProfile(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	vaulttest.SetRecord(t, "copilot", "profile", "copilot")
	vaulttest.Secret(t, providers.SecretName("databricks", "qa", providers.AuthKey), "fixture-key")
	if got := fallbackRow(d, "copilot").desc; !strings.Contains(got, "databricks-qa") || !strings.Contains(got, "no credits") {
		t.Errorf("automatic fallback = %q", got)
	}
	if _, err := writeAgentField(d, "copilot", "FALLBACK", "prod"); err != nil {
		t.Fatal(err)
	}
	if got := fallbackRow(d, "copilot").desc; !strings.Contains(got, "prod") || !strings.Contains(got, "profile fails") || strings.Contains(got, "databricks-qa") {
		t.Errorf("explicit fallback = %q", got)
	}
}

func TestDatabricksSignedInDevAppearsAlongsideQaAndProd(t *testing.T) {
	menuTestEnv(t)
	d := worstCaseHomeDialog(t)
	vaulttest.Secret(t, providers.AuthSecretName(d.reg.Providers["dev"]), `{"refresh":"fixture-refresh"}`)
	for _, a := range setupAgentNames {
		_, out := pickWorkspace(t, d, a, "1\n", mintM2M)
		if !strings.Contains(out, "1) dev") || !strings.Contains(out, "2) qa") || !strings.Contains(out, "3) prod") {
			t.Fatalf("%s picker = %q, want dev, qa and prod", a, out)
		}
		if vault.GetAgentRecord(a).Profile != "dev" {
			t.Fatalf("%s did not pin dev", a)
		}
	}
}
