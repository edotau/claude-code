package menu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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
	if r := vault.GetAgentRecord("codex"); r.Route.Profile != "chatgpt" || r.Model != "keep-me" {
		t.Errorf("record after own-login pick = %+v, want profile chatgpt, model kept", r)
	}
	runProfile(t, reg, "codex", "1\n")
	if r := vault.GetAgentRecord("codex"); r.Route.Profile != "" || r.Model != "keep-me" {
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
	if got := vault.GetAgentRecord("opencode").Route.Profile; got != opencode.OwnLogin {
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
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "chatgpt" {
		t.Fatalf("off codex after sign-in = %q, want chatgpt", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Route: route.Route{Profile: "openai"}}); err != nil {
		t.Fatal(err)
	}
	agentActiveOn(d, "codex")
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "openai" {
		t.Errorf("chosen profile overridden: %q, want openai", got)
	}
}

func seedDialog(t *testing.T, script string) (*dialog, *bytes.Buffer) {
	t.Helper()
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	if got := vault.AgentRecords(agentpkg.RosterNames())["claude"].Route.Profile; got != "" {
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
		if r.Route.Profile != "" {
			t.Errorf("%s wrote profile %q on dismiss", a, r.Route.Profile)
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
	if recs["copilot"].Route.Profile != want || recs["codex"].Route.Profile != "" {
		t.Errorf("records = copilot %q codex %q, want %q and off", recs["copilot"].Route.Profile, recs["codex"].Route.Profile, want)
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
		if r.Route.Profile != "" {
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
	if got := vault.AgentRecords(agentpkg.RosterNames())["copilot"].Route.Profile; got != profiles[0] || !d.changed {
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
	if got := vault.GetAgentRecord("copilot"); got.Route.Profile != "copilot" || got.Model != "keep-model" {
		t.Fatalf("refused pin must keep the record: %+v", got)
	}
}

func TestSaveSeedMarksDialogChanged(t *testing.T) {
	d, out := seedDialog(t, "")
	if !saveSeed(d, map[string]string{"codex": "chatgpt"}) {
		t.Fatalf("saveSeed failed: %s", out)
	}
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "chatgpt" {
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
		if r.Route.Profile != "" {
			t.Errorf("%s persisted profile %q after a failed save", a, r.Route.Profile)
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
		m[a] = vault.GetAgentRecord(a).Route.Profile
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
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "prod" {
		t.Errorf("page agent codex on own login: profile = %q, want prod", got)
	}
	if got := vault.GetAgentRecord("opencode").Route.Profile; got != "prod" {
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
		if got := vault.GetAgentRecord("codex").Route.Profile; got != tc.pick {
			t.Errorf("pick %s: codex = %q", tc.pick, got)
		}
		if got := vault.GetAgentRecord("opencode").Route.Profile; got != tc.opencode {
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
	if got := vault.GetAgentRecord("codex").Route.Profile; got != "qa" {
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
	d.reg = addGithubProfiles(t, "octo")
	copilotLoggedInAs(t, "octo")
	for _, a := range setupAgentNames {
		vaulttest.SetRecord(t, a, "profile", "qa")
		rows := []billingRow{{label: "Login", run: func() bool { return false }}}
		activateOnSignIn(d, a, rows)
		if rows[0].run() || vault.GetAgentRecord(a).Route.Profile != "qa" {
			t.Fatalf("%s failed sign-in changed the workspace pin", a)
		}
		rows = []billingRow{{label: "Login", run: func() bool { return true }}}
		activateOnSignIn(d, a, rows)
		if !rows[0].run() {
			t.Fatalf("%s own login failed", a)
		}
		want := signInProfile(d, a)
		if got := vault.GetAgentRecord(a).Route.Profile; got != want || got == "qa" || got == "" {
			t.Errorf("%s profile = %q, want own login %q", a, got, want)
		}
	}
}

// addGithubProfiles adds one github profile per login and returns the reloaded registry.
func addGithubProfiles(t *testing.T, logins ...string) *providers.Registry {
	t.Helper()
	for _, u := range logins {
		if err := providers.AddGithubAccount(u); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// copilotLoggedInAs writes copilot's own stored login into a fresh COPILOT_HOME; "" leaves copilot logged out.
func copilotLoggedInAs(t *testing.T, login string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("COPILOT_HOME", dir)
	if login == "" {
		return
	}
	body := "// written by copilot\n{\"lastLoggedInUser\":{\"login\":\"" + login + "\"}}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A copilot sign-in pins the github profile of copilot's own login, never the first one, and keeps an account pin.
func TestCopilotSignInPinsItsOwnLogin(t *testing.T) {
	for _, tc := range []struct{ name, pin, login, want string }{
		{"own login pinned, not the first profile", "", "xuser", "xuser"},
		{"no matching profile pins nothing", "", "nobody", ""},
		{"logged out pins nothing", "", "", ""},
		{"explicit account pin kept", "auser", "xuser", "auser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			menuTestEnv(t)
			reg := addGithubProfiles(t, "auser", "xuser")
			vaulttest.SetRecord(t, "copilot", "profile", tc.pin)
			copilotLoggedInAs(t, tc.login)
			d := &dialog{reg: reg, out: &bytes.Buffer{}}
			rows := []billingRow{{label: "Login", run: func() bool { return true }}}
			activateOnSignIn(d, "copilot", rows)
			if !rows[0].run() {
				t.Fatalf("sign-in reported failure: %s", d.out)
			}
			if got := vault.GetAgentRecord("copilot").Route.Profile; got != tc.want {
				t.Errorf("copilot profile = %q, want %q; dialog said %q", got, tc.want, d.out)
			}
		})
	}
}

// gh's active account is not copilot's login: the page and Set up see copilot as not signed in.
func TestCopilotGHActiveIsNotCopilotsLogin(t *testing.T) {
	reg := menuTestEnv(t)
	fakeGHActive(t)
	copilotLoggedInAs(t, "")
	if ok, _ := signedIn("copilot"); ok {
		t.Error("signedIn(copilot) = true from gh's active account")
	}
	if st := copilotMenu(&dialog{reg: reg, out: discardWriter{}}).state; st.verified || st.mode == modeSubscription {
		t.Errorf("copilot state = %+v, want not verified", st)
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
		if vault.GetAgentRecord(a).Route.Profile != "dev" {
			t.Fatalf("%s did not pin dev", a)
		}
	}
}

// The Model row refuses a model no row serves and writes nothing.
func TestWriteAgentFieldRefusesUnservedModel(t *testing.T) {
	vaulttest.Use(t)
	d := &dialog{out: &bytes.Buffer{}}
	if changed, err := writeAgentField(d, "codex", "MODEL", "bogus:opus"); changed || err == nil {
		t.Errorf("changed=%v err=%v, want a refusal", changed, err)
	}
	if got := vault.GetAgentRecord("codex").Model; got != "" {
		t.Errorf("model %q written, want nothing", got)
	}
}

// The Model picker lists default, the discovered ids and the typed row (width-checked); a pick writes MODEL.
func TestModelPickerListsDiscoveredAndWrites(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	orig := discoverModels
	t.Cleanup(func() { discoverModels = orig })
	discoverModels = func(context.Context, configure.ModelQuery) (configure.ModelResult, error) {
		return configure.ModelResult{Models: []configure.DiscoveredModel{{ID: "gpt-x"}, {ID: "gpt-y"}}}, nil
	}
	ids, _ := modelIDs(&dialog{reg: reg}, "codex", "chatgpt", configure.RecallModels)
	assertFitsEighty(t, "model picker", modelPickRows(ids, "gpt-y", true))
	out := &bytes.Buffer{}
	in := strings.NewReader("3\n")
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: out}
	recordModelRow(d, "codex").run()
	if !strings.Contains(out.String(), "gpt-x") || vault.GetAgentRecord("codex").Model != "gpt-y" {
		t.Errorf("picker %q, model %q, want gpt-x listed and gpt-y written", out.String(), vault.GetAgentRecord("codex").Model)
	}
}

func TestBillingLabel(t *testing.T) {
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := billingLabel(reg, "ultra"); got != "subscription" {
		t.Errorf("billingLabel(ultra) = %q", got)
	}
	if got := billingLabel(reg, "anthropic"); got != "API key" {
		t.Errorf("billingLabel(anthropic) = %q", got)
	}
	if got := billingLabel(reg, "chatgpt"); got != "openai-chatgpt · subscription" {
		t.Errorf("billingLabel(chatgpt) = %q", got)
	}
}

func codexSettingsFixture(t *testing.T) (string, string) {
	t.Helper()
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# native preferences\nmodel_verbosity = \"high\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

// putCodexRecord stores rec as the codex agent record.
func putCodexRecord(rec vault.AgentRecord) error {
	return vault.Update(func(tx *vault.Tx) error {
		return tx.PutAgentRecords(map[string]vault.AgentRecord{"codex": rec})
	})
}

func codexSettingsDialog(home, input string) (*dialog, *bytes.Buffer) {
	in := strings.NewReader(input)
	out := &bytes.Buffer{}
	return &dialog{o: Options{Home: home, In: in}, br: bufio.NewReader(in), out: out}, out
}

// readBytes reads path or fails the test.
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeOthers writes each path's contents (creating parents) so a test can prove other agents' files stay untouched.
func writeOthers(t *testing.T, others map[string]string) {
	t.Helper()
	for p, contents := range others {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// assertFilesUnchanged requires every file in others to still hold its seeded contents.
func assertFilesUnchanged(t *testing.T, others map[string]string) {
	t.Helper()
	for p, want := range others {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Errorf("other agent file changed: %s (err=%v)", p, err)
		}
	}
}

// assertCodexSaved requires the saved preference in the overlay and native file, with the original backed up.
func assertCodexSaved(t *testing.T, path string, before, after []byte, ov map[string]string) {
	t.Helper()
	if ov["model_verbosity"] != "low" || !strings.Contains(string(after), `model_verbosity = "low"`) {
		t.Fatalf("saved preference missing: overlay=%v native=%s", ov, after)
	}
	backup, found, err := codex.LatestCodexBackup(path)
	if err != nil || !found || !bytes.Equal(backup.Data, before) {
		t.Fatalf("original native settings not backed up: found=%v err=%v", found, err)
	}
}

func TestCodexSettingsMenuStagesUntilSave(t *testing.T) {
	for _, save := range []bool{false, true} {
		name, input := "cancel", "2\n1\n\n"
		if save {
			name, input = "save", "2\n1\n4\n"
		}
		t.Run(name, func(t *testing.T) {
			home, path := codexSettingsFixture(t)
			before := readBytes(t, path)
			others := map[string]string{
				filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"): "{\"foreign\":true}\n",
				filepath.Join(home, ".copilot", "settings.json"):               "{\"theme\":\"dim\"}\n",
				filepath.Join(home, ".gemini", "settings.json"):                "{\"foreign\":true}\n",
			}
			writeOthers(t, others)
			d, out := codexSettingsDialog(home, input)
			if !runCodexSettingsMenu(d, map[string]string{}, map[string]string{"model_verbosity": "high"}) {
				t.Fatalf("menu failed: %s", out.String())
			}
			ov, err := codex.LoadCodexPreferences()
			if err != nil {
				t.Fatal(err)
			}
			after := readBytes(t, path)
			if save {
				assertCodexSaved(t, path, before, after, ov)
			} else if len(ov) != 0 || !bytes.Equal(before, after) {
				t.Fatalf("cancel persisted staged edit: overlay=%v native=%s", ov, after)
			}
			if d.changed {
				t.Error("Codex-only save must not request a global agent rerender")
			}
			assertFilesUnchanged(t, others)
		})
	}
}

func TestCodexSettingsReportsNeverWrite(t *testing.T) {
	for _, mode := range []string{"report", "check", "dry-run", "piped"} {
		t.Run(mode, func(t *testing.T) {
			home, path := codexSettingsFixture(t)
			if err := putCodexRecord(vault.AgentRecord{Extras: map[string]string{"model_verbosity": "low"}}); err != nil {
				t.Fatal(err)
			}
			nativeBefore, vaultBefore := readBytes(t, path), readBytes(t, vault.Path())
			input := strings.NewReader("2\n1\n4\n")
			var out bytes.Buffer
			o := Options{Home: home, In: input, Out: &out, Check: mode == "check", DryRun: mode == "dry-run", Interactive: mode == "piped"}
			rc := Run(o, "codex")
			want := 0
			if mode == "check" {
				want = 1
			}
			if rc != want || !strings.Contains(out.String(), "live differs") {
				t.Fatalf("report rc=%d want=%d output=%s", rc, want, out.String())
			}
			if input.Len() != len("2\n1\n4\n") {
				t.Error("read-only report consumed menu input")
			}
			if !bytes.Equal(nativeBefore, readBytes(t, path)) || !bytes.Equal(vaultBefore, readBytes(t, vault.Path())) {
				t.Error("read-only report changed native config or vault")
			}
		})
	}
}

func TestCodexSettingsApplyFailureIsReported(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".codex"), []byte("blocked native directory"), 0600); err != nil {
		t.Fatal(err)
	}
	d, out := codexSettingsDialog(home, "2\n1\n4\n")
	if runCodexSettingsMenu(d, map[string]string{}, map[string]string{}) {
		t.Fatal("menu reported success after native apply failed")
	}
	if !strings.Contains(out.String(), "preferences saved; native apply failed") {
		t.Fatalf("missing partial-save explanation: %s", out.String())
	}
	ov, err := codex.LoadCodexPreferences()
	if err != nil || ov["model_verbosity"] != "low" {
		t.Fatalf("saved preference unavailable after apply failure: %v, %v", ov, err)
	}
	out.Reset()
	if rc := Run(Options{Home: home, In: strings.NewReader(""), Out: out}, "codex"); rc != 1 {
		t.Fatalf("native read failure rc=%d want=1: %s", rc, out.String())
	}
}

func TestCodexSettingsCheckDetectsUnappliedReset(t *testing.T) {
	home, path := codexSettingsFixture(t)
	if err := putCodexRecord(vault.AgentRecord{Extras: map[string]string{"model_verbosity": "low"}}); err != nil {
		t.Fatal(err)
	}
	if err := codex.ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	// A failed native reset leaves the new empty overlay saved and the old owned preference on disk.
	if err := putCodexRecord(vault.AgentRecord{}); err != nil {
		t.Fatal(err)
	}
	nativeBefore, vaultBefore := readBytes(t, path), readBytes(t, vault.Path())
	var out bytes.Buffer
	o := Options{Home: home, Check: true, In: strings.NewReader(""), Out: &out}
	if rc := Run(o, "codex"); rc != 1 {
		t.Fatalf("unapplied reset rc=%d want=1: %s", rc, out.String())
	}
	if !bytes.Equal(nativeBefore, readBytes(t, path)) || !bytes.Equal(vaultBefore, readBytes(t, vault.Path())) {
		t.Fatal("check of unapplied reset changed native config or vault")
	}
	if err := codex.ApplyCodexSettings(home); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if rc := Run(o, "codex"); rc != 0 {
		t.Fatalf("applied reset rc=%d want=0: %s", rc, out.String())
	}
}

func TestWebSearchCodexHomeNoPlaintextBackup(t *testing.T) {
	cfgDir := vaulttest.Use(t)
	home, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	custom := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(custom, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := codexWebSearchRegister(t)
	if err := reg("/bin/claude-code"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(custom)
	if !strings.Contains(string(got), "[mcp_servers.web_search]") {
		t.Errorf("$CODEX_HOME/config.toml not edited: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err == nil {
		t.Error("~/.codex/config.toml was written despite CODEX_HOME")
	}
	if leftovers := plaintextLeftovers(cfgDir, codexHome); len(leftovers) > 0 {
		t.Errorf("plaintext backup left behind: %v", leftovers)
	}
}

// codexWebSearchRegister is the codex web-search client's register func.
func codexWebSearchRegister(t *testing.T) func(string) error {
	t.Helper()
	for _, c := range webSearchClients() {
		if c.name == "codex" {
			return c.register
		}
	}
	t.Fatal("no codex client")
	return nil
}

// plaintextLeftovers lists config.toml copies under cfgDir and every non-config.toml entry of codexHome.
func plaintextLeftovers(cfgDir, codexHome string) []string {
	var leftovers []string
	filepath.WalkDir(cfgDir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && strings.Contains(filepath.Base(p), "config.toml") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	entries, _ := os.ReadDir(codexHome)
	for _, e := range entries {
		if e.Name() == "config.toml" || e.Name() == "config.toml.lock" && emptyFile(filepath.Join(codexHome, e.Name())) {
			continue
		}
		leftovers = append(leftovers, e.Name())
	}
	return leftovers
}

// emptyFile reports whether p is a zero-length regular file.
func emptyFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == 0
}
