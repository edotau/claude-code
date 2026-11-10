package menu

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runKeysCheck drives the read-only (--check/piped) keys report; !allOK maps to rc 1.
func runKeysCheck(t *testing.T, o Options) (out bytes.Buffer, rc int) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load: %v", err)
	}
	o.In = o.reader()
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: &out}
	d.header("keys")
	allOK, _ := runKeysChanged(d)
	if !allOK {
		rc = 1
	}
	return out, rc
}

// A piped --check on a box with no keys reports every absent credential, writes nothing, and exits 1.
func TestCheckPipedReportsAbsentKeysAndExitsOne(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	vaulttest.Use(t) // pins are vault-only: seed the openai pin, then assert --check leaves the vault bytes alone
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	putRec(t, "claude", "PROFILE", "openai")
	vaultBefore, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db"))
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no gh/codex/copilot on PATH: nothing can look "present" by accident
	out, rc := runKeysCheck(t, Options{Check: true, Home: home, In: strings.NewReader("")})
	if rc != 1 {
		t.Errorf("rc = %d, want 1; output:\n%s", rc, out.String())
	}
	for _, want := range []string{"== keys ==", "ABSENT", "claude-code vault set OPENAI_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if after, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db")); !bytes.Equal(after, vaultBefore) {
		t.Error("--check wrote vault.db")
	}
}

// A vendor whose only resolvable provider is another vendor's passthrough login is ABSENT, not "present".
func TestVendorKeyTargetRejectsForeignPassthrough(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	out, _ := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if !strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "codex      present (subscription") {
		t.Errorf("codex must not read as present on a subscription session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "codex login") {
		t.Errorf("codex row should name `codex login` as the fix:\n%s", out.String())
	}
}

// A vendor running on its own stored config (opencode.jsonc sets `model`) is configured, not ABSENT: the runner
// launches it untouched, so `--check` must not demand a key for it.
// opencode's own opencode.jsonc "model" setting is no longer a fallback dispatch (DescribeRoute's
// copilot-login -> anthropic-key -> none chain replaced it): with no credential on
// that chain, the vendor reads ABSENT and the fix command names `claude-code configure github`.
func TestVendorOnOwnConfigReadsAbsentUnderNewChain(t *testing.T) {
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "opencode", "opencode.jsonc"), []byte(`{"model": "google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "opencode", Home: home, In: strings.NewReader("")})
	if rc != 1 || !strings.Contains(out.String(), "opencode") || !strings.Contains(out.String(), "ABSENT") {
		t.Errorf("rc = %d; opencode.jsonc's own model must no longer read present:\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "google/gemini-3.8-flash") {
		t.Errorf("the retired own-config dispatch must not appear in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "claude-code configure github") {
		t.Errorf("fix command must name `claude-code configure github`:\n%s", out.String())
	}
}

// An absent credential offers BOTH the browser login and a pasted key; piped, each is one [Y/n], so declining
// the login and accepting the key seals it in the vault without opening a browser.
func TestResolveKeyOffersLoginAndKeyThenStoresPastedKey(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	kt := configure.KeyTarget{Label: "openai", EnvVar: "OPENAI_API_KEY", LoginArgv: []string{"openai", "login"}, FixCmd: "x"}
	opts := configure.AuthOptions(kt)
	if len(opts) != 2 || opts[0].Argv == nil || opts[1].Env != "OPENAI_API_KEY" || !strings.Contains(opts[1].Desc, "platform.openai.com") {
		t.Fatalf("authOptions = %+v, want browser login then key with its console URL", opts)
	}
	in := strings.NewReader("n\ny\nn\n  sk-test-value \n")
	var out bytes.Buffer
	d := &dialog{o: Options{Interactive: true, In: in, Out: &out}, br: bufio.NewReader(in), out: &out}
	if !resolveKey(d, kt) {
		t.Fatalf("resolveKey = false\n%s", out.String())
	}
	got, src, ok := vault.Get("OPENAI_API_KEY")
	if !ok || src != "vault" || got != "sk-test-value" {
		t.Errorf("stored key = (%q, %q, %v)", got, src, ok)
	}
	assertVaultRecordSealed(t, "OPENAI_API_KEY", "sk-test-value")
	if strings.Contains(out.String(), "logged in") {
		t.Error("login must not have run after being declined")
	}
	assertContainsAll(t, out.String(), []string{"Browser login — openai login? [Y/n]", "Paste API key — create at platform.openai.com/api-keys"})
}

// discardWriter is an io.Writer that throws away everything written to it (dialog.out needs one when
// a test never asserts on dialog output).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// assertContainsAll requires every want to appear in the dialog output got.
func assertContainsAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("dialog lacks %q:\n%s", want, got)
		}
	}
}

// assertVaultRecordSealed requires the secret record for name not to contain the plaintext secret.
func assertVaultRecordSealed(t *testing.T, name, secret string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		if raw, _ := tx.Get(vault.Secrets, name); strings.Contains(raw, secret) {
			t.Error("vault record contains the plaintext secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --check on a present, rotatable credential (codex's ChatGPT login) must stay read-only: no "keep it?"
// prompt, no rotate CLI invoked, no file write — the new rotate feature must not change --check's contract.
func TestCheckStaysReadOnlyWithRotatableCredential(t *testing.T) {
	cfg, home, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check, never execs a CLI
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (codex reads present, nothing to fix):\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "current credential as-is") || strings.Contains(out.String(), "logged in again") {
		t.Errorf("--check must never prompt to rotate or run a rotate CLI:\n%s", out.String())
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "vault.db")); err == nil {
		t.Error("--check wrote vault.db")
	}
}

// `keys` was removed from Sections: naming it as a section now reports "unknown section" and exits 2,
// never falling through to the key report (that report still runs internally, via Vendor -> runKeysChanged).
func TestRunUnknownSectionKeysReturnsTwo(t *testing.T) {
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "keys")
	if rc != 2 {
		t.Errorf("rc = %d, want 2:\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "unknown section") {
		t.Errorf("output lacks %q:\n%s", "unknown section", out.String())
	}
}

// assertMaxPlanOptions requires the Max-plan row's three ways in: login, mint, paste.
func assertMaxPlanOptions(t *testing.T, opts []configure.AuthOption) {
	t.Helper()
	if len(opts) != 3 || opts[0].Argv == nil || opts[1].Mint == nil || opts[1].Env != "CLAUDE_CODE_OAUTH_TOKEN" || opts[2].Env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("authOptions = %+v, want login, mint, paste", opts)
	}
}

// The Max-plan row offers three ways in: browser login, minting a headless token, pasting one; the minted token
// is recognised in the CLI's output by its sk-ant-oat prefix.
func TestMaxPlanOptionsAndSetupTokenCapture(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: status reads as not logged in
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	kt := configure.MaxPlanTarget(configure.KeyTarget{Label: "claude"})
	if kt.Present || kt.Headless {
		t.Fatalf("no login and no token must read absent: %+v", kt)
	}
	assertMaxPlanOptions(t, configure.AuthOptions(kt))
	out := "Opening browser...\nPaste the code here:\n\nYour token: sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789\n"
	if got := configure.SetupTokenRE.FindString(out); got != "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789" {
		t.Errorf("token capture = %q", got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored")
	if kt := configure.MaxPlanTarget(configure.KeyTarget{Label: "claude"}); !kt.Present || !kt.Headless || !strings.Contains(kt.Source, "headless token") {
		t.Errorf("a stored token must read present/headless: %+v", kt)
	}
}

// copilot present via a raw env token override offers no rotate CLI (`copilot login` would rotate
// copilot's own session, not the override); present via copilot's/gh's own login does.
func TestVendorKeyTargetCopilotRotateWiring(t *testing.T) {
	t.Setenv("PATH", t.TempDir())              // no real gh/secret-tool on PATH
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // CLAUDE_CONFIG_DIR isolated so nothing real leaks in
	t.Setenv("COPILOT_HOME", t.TempDir())      // no real copilot config.json: CopilotStoredLogin reads nothing
	t.Setenv("GH_CONFIG_DIR", t.TempDir())     // no real gh hosts.yml: GHActiveAccount reads nothing
	t.Setenv("GH_TOKEN", "ghp_env_override")
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := configure.AgentKeyTarget(reg, "copilot")
	if !kt.Present || len(kt.LoginArgv) != 0 {
		t.Errorf("env-token copilot = %+v, want present with no loginArgv", kt)
	}
}

// codex present via its own ChatGPT login (file-based, no codex binary needed) offers `codex login` as the
// rotate CLI.
func TestVendorKeyTargetCodexRotateWiring(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := configure.AgentKeyTarget(reg, "codex")
	if !kt.Present {
		t.Fatalf("codex = %+v, want present via the ChatGPT login file", kt)
	}
	if len(kt.LoginArgv) != 2 || kt.LoginArgv[0] != "codex" || kt.LoginArgv[1] != "login" {
		t.Errorf("codex loginArgv = %v, want [codex login]", kt.LoginArgv)
	}
}

// gemini present via agy's cached Google login (~/.gemini/antigravity-cli/antigravity-oauth-token) offers
// `agy` as the rotate CLI.
func TestVendorKeyTargetGeminiGoogleLoginRotateWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	kt := configure.AgentKeyTarget(reg, "gemini")
	if !kt.Present {
		t.Fatalf("gemini = %+v, want present via the agy login marker", kt)
	}
	if len(kt.LoginArgv) != 1 || kt.LoginArgv[0] != "agy" {
		t.Errorf("gemini loginArgv = %v, want [agy]", kt.LoginArgv)
	}
}

// runRealRoute drives Set up's real route step (picker + pin write) after a verified agent, answering the picker with in.
func runRealRoute(t *testing.T, in string, readOnlyVault bool) string {
	t.Helper()
	reg := menuTestEnv(t)
	putRec(t, "codex", "PROFILE", "chatgpt") // codex is on its own login, so picking off is a real write
	if readOnlyVault {
		if err := os.Chmod(vault.Path(), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(vault.Path(), 0o600) })
	}
	st, ve := setupState, setupVerify
	t.Cleanup(func() { setupState, setupVerify = st, ve })
	setupState = func(*dialog, string) agentState { return agentState{verified: true} }
	setupVerify = func(context.Context, string) error { return nil }
	var out bytes.Buffer
	d := &dialog{reg: reg, out: &out, br: bufio.NewReader(strings.NewReader(in))}
	agentSetUp(d, "codex")
	return out.String()
}

// A pin write that fails must end Set up with "stopped at route", not read as a dismissed picker.
func TestSetUpRouteFailedPinWriteIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	got := runRealRoute(t, "1\n", true)
	if !strings.Contains(got, "set up stopped at route:") || strings.Contains(got, "the route was not changed") {
		t.Errorf("failed write: out = %q, want 'stopped at route' and no 'not changed'", got)
	}
}

// A dismissed picker stays quiet: only "the route was not changed", no error.
func TestSetUpRouteCancelIsQuiet(t *testing.T) {
	got := runRealRoute(t, "\n", false)
	if !strings.Contains(got, "signed in; the route was not changed") || strings.Contains(got, "stopped at") {
		t.Errorf("cancel: out = %q, want only 'route was not changed'", got)
	}
}

// maxRuneLen is the widest label (rune count) among rows a picker renders together.
func maxRuneLen(labels []string) int {
	w := 0
	for _, l := range labels {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	return w
}

// rowRenderedWidth mirrors tui's layoutRows + render: a 2-char marker/space prefix, every label padded to
// the widest in its row set, a 2-space spacer, then the detail column.
func rowRenderedWidth(labels []string, detail string) int {
	return 2 + maxRuneLen(labels) + 2 + len([]rune(ansi.Strip(detail)))
}

// TestMenuRowWidthsFitEightyColumns: every row configure shows — the top agent picker and each agent's own
// submenu — must render at 80 columns or fewer; past that it wraps on an ordinary terminal, which (per the
// tui package's own wrapped-line accounting) is exactly what desyncs a redraw's rewind count.
func TestMenuRowWidthsFitEightyColumns(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}

	rows, _ := menuRows(d)
	rows = append(rows, submitRow)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	for _, r := range rows {
		if w := rowRenderedWidth(labels, r[1]); w > 80 {
			t.Errorf("top menu row %q = %d cols, want <=80: %q", r[0], w, r[1])
		}
	}

	for name, build := range agentMenus {
		m := build(d)
		var subLabels []string
		for _, r := range m.rows {
			subLabels = append(subLabels, r.label)
		}
		subLabels = append(subLabels, "Back")
		for _, r := range m.rows {
			if w := rowRenderedWidth(subLabels, r.desc); w > 80 {
				t.Errorf("%s submenu row %q = %d cols, want <=80: %q", name, r.label, w, r.desc)
			}
		}
	}
}

// TestHomeExtraRowsFitEightyColumns renders the tools/settings rows with worst-case state text.
func TestHomeExtraRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	rows := []homeRow{
		{Name: "tailscale", State: "not connected", Check: "a-long-hostname-01 · no admin key", Run: nil},
		{Name: "web search", State: "no key", Check: "mcp: claude,codex", Run: nil},
		{Name: "browsers", State: "default chrome", Check: "12 installed", Run: nil},
		{Name: "databricks", State: "prod · oauth-m2m", Check: "expires in 119m", Run: nil},
		{Name: "databricks", State: "prod · oauth-m2m", Check: "expires in 23h59m", Run: nil},
	}
	rows = append(rows, settingsRows(&dialog{})...)
	laid, _ := RowsOf(rows)
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
}

// TestRoutingRowFitsEightyColumns: the widest "routing" page row — the longest field key, its longest
// value, plus the "default"/"vault" source suffix — stays <=80 cols.
func TestRoutingRowFitsEightyColumns(t *testing.T) {
	var labels []string
	for _, m := range settings.Modules() {
		for _, f := range m.Fields() {
			labels = append(labels, f.Key)
		}
	}
	worst := providers.Routing{
		Weights: map[string]float64{"price": 0.123456789, "headroom": 0.876543211},
		Opencode: providers.OpencodeRouting{
			Ladder:          &providers.RouteOpencodeSubIDs,
			WorkRepoMarkers: &[]string{"gitlab.example.com", "github.example.net"},
			FreeTiers:       &[]string{"sonnet", "haiku"},
			FreeFirstOutput: "90s",
		},
	}
	for _, m := range settings.Modules() {
		for _, f := range m.Fields() {
			detail := f.Get(worst) + "  " + routingSource(f, worst)
			if w := rowRenderedWidth(labels, detail); w > 80 {
				t.Errorf("routing field %s = %d cols, want <=80: %q", f.Key, w, detail)
			}
		}
	}
}

// TestTierRowsFitEightyColumns: alias · use · a long model id stays within 80 columns.
func TestTierRowsFitEightyColumns(t *testing.T) {
	for alias, use := range aliasUse {
		row := fmt.Sprintf("  %-6s  %s", alias, PadTo(use, 42)+"anthropic/claude-sonnet-5-5") // as tui.layoutRows lays it
		if w := len([]rune(row)); w > 80 {
			t.Errorf("%s alias row is %d columns: %q", alias, w, row)
		}
	}
}

// TestDatabricksRowsFitEightyColumns: the live home row and every page/env/apply row stay <=80 cols.
func TestDatabricksRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	d := dbxEnv(t)
	if row := databricksRow(d); 2+len([]rune("environment"))+2+len([]rune(row.State))+2+len([]rune(row.Check)) > 80 {
		t.Errorf("live databricks row too wide: %+v", row)
	}
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{1, 4, 0, -1, -1}
	runDatabricksPage(d)
	for i, set := range f.rows {
		var labels []string
		for _, r := range set {
			labels = append(labels, r[0])
		}
		for _, r := range set {
			if w := rowRenderedWidth(labels, r[1]); w > 80 {
				t.Errorf("%s row %q = %d cols: %q", f.titles[i], r[0], w, r[1])
			}
		}
	}
	worst := "prod"
	if w := rowRenderedWidth([]string{"dev", "qa", worst}, "oauth-m2m  token expires in 23h59m"); w > 80 {
		t.Errorf("env row worst case = %d cols", w)
	}
}

func TestSecretLineEchoesMaskedPreview(t *testing.T) {
	for _, in := range []string{"tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n",
		"TAILSCALE_OAUTH_CLIENT_SECRET=tskey-client-kAbCdEf123CNTRL-0123456789abcdefghij\n"} {
		var out strings.Builder
		d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader(in))}
		if _, err := secretLine(d, "> "); err != nil {
			t.Fatal(err)
		}
		if w := len([]rune(strings.Split(strings.TrimSpace(out.String()), "\n")[1])); w > 80 {
			t.Errorf("preview line is %d columns", w)
		}
		if strings.Contains(out.String(), "0123456789") || !strings.Contains(out.String(), "(49 chars)") {
			t.Errorf("preview leaks or lacks length: %q", out.String())
		}
	}
}

func TestSecretLineFallbackPreservesInputAndOutput(t *testing.T) {
	for _, in := range []string{"", "\n", "  plain value  \r\n", "NAME=private-value\n", "unterminated-value"} {
		t.Run(strings.TrimSpace(in), func(t *testing.T) {
			d, out := newDialog(in)
			got, err := secretLine(d, "> ")
			want := strings.TrimRight(in, "\r\n")
			if err != nil || got != want {
				t.Fatalf("secretLine = (%q, %v), want (%q, nil)", got, err, want)
			}
			wantOut := "> \n"
			if _, value := configure.ParseAssignment(want); value != "" {
				wantOut += "  pasted  " + pastePreview(want) + "\n"
			}
			if out.String() != wantOut {
				t.Errorf("output = %q, want %q", out.String(), wantOut)
			}
		})
	}
}

// TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys: y stores the page's staged keys (Submit has none left),
// N restores env; a key staged before the page opened is untouched either way.
func TestConfirmPageKeysSavesOrDiscardsOnlyThePagesKeys(t *testing.T) {
	for _, yes := range []bool{true, false} {
		menuTestEnv(t)
		t.Setenv("OLD_KEY", "")
		os.Unsetenv("PAGE_KEY")
		d := &dialog{out: discardWriter{}, pending: map[string]stagedKey{}}
		d.stage("OLD_KEY", "old")
		before := map[string]stagedKey{"OLD_KEY": d.pending["OLD_KEY"]}
		d.stage("PAGE_KEY", "page-value")
		orig := saveConfirmed
		saveConfirmed = func(string) bool { return yes }
		confirmPageKeys(d, before)
		saveConfirmed = orig
		if _, still := d.pending["PAGE_KEY"]; still {
			t.Errorf("yes=%v: PAGE_KEY still pending after the page closed", yes)
		}
		if _, ok := d.pending["OLD_KEY"]; !ok {
			t.Errorf("yes=%v: OLD_KEY (staged before the page) must stay for Submit", yes)
		}
		if got := os.Getenv("PAGE_KEY"); (got == "page-value") != yes {
			t.Errorf("yes=%v: PAGE_KEY env = %q", yes, got)
		}
		d.pending = nil
		os.Unsetenv("PAGE_KEY")
	}
}
