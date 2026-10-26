package configure

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runKeysCheck drives runKeysChanged directly the way Run's Sections walk used to before `keys` was
// removed from Sections: --check/piped is still read-only here (resolveKey/runAuthOption never fire
// for an absent key when o.Check or !o.Interactive), so this reproduces the old `Run(opts, "keys")`
// behavior without going through the removed section name. !allOK maps to the CLI's old rc 1.
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
	t.Setenv("HARNESS_PROVIDER", "openai")
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
	if _, err := os.Stat(filepath.Join(cfg, "state", "vault.db")); err == nil {
		t.Error("--check wrote vault.db")
	}
}

// A vendor whose only resolvable provider is another vendor's passthrough login is ABSENT, not "present".
func TestVendorKeyTargetRejectsForeignPassthrough(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("HARNESS_PROVIDER", "subscription")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	out, _ := runKeysCheck(t, Options{Check: true, Vendor: "codex", Home: home, In: strings.NewReader("")})
	if !strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "codex      present (subscription") {
		t.Errorf("codex must not read as present on a subscription session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "codex login") {
		t.Errorf("codex row should name `codex login` as the fix:\n%s", out.String())
	}
}

// A vendor running on its own stored config (opencode.jsonc sets `model`) is configured, not ABSENT: the runner
// launches it untouched, so `--check` must not demand a key for it.
// opencode's own opencode.jsonc "model" setting is no longer a fallback leg (DescribeRoute's
// copilot-login -> anthropic-key -> none chain replaced it): with no credential on
// that chain, the vendor reads ABSENT and the fix command names `claude-code configure github`.
func TestVendorOnOwnConfigReadsAbsentUnderNewChain(t *testing.T) {
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "subscription")
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
	out, rc := runKeysCheck(t, Options{Check: true, Vendor: "opencode", Home: home, In: strings.NewReader("")})
	if rc != 1 || !strings.Contains(out.String(), "opencode") || !strings.Contains(out.String(), "ABSENT") {
		t.Errorf("rc = %d; opencode.jsonc's own model must no longer read present:\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "google/gemini-3.8-flash") {
		t.Errorf("the retired own-config leg must not appear in output:\n%s", out.String())
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
	kt := keyTarget{label: "openai", envVar: "OPENAI_API_KEY", loginArgv: []string{"openai", "login"}, fixCmd: "x"}
	opts := authOptions(kt)
	if len(opts) != 2 || opts[0].argv == nil || opts[1].env != "OPENAI_API_KEY" || !strings.Contains(opts[1].desc, "platform.openai.com") {
		t.Fatalf("authOptions = %+v, want browser login then key with its console URL", opts)
	}
	in := strings.NewReader("n\ny\nn\n  sk-test-value \n")
	var out bytes.Buffer
	d := &dialog{o: Options{Interactive: true, In: in, Out: &out}, br: bufio.NewReader(in), out: &out}
	if !resolveKey(d, kt) {
		t.Fatalf("resolveKey = false\n%s", out.String())
	}
	got, src, ok := secrets.Get("OPENAI_API_KEY")
	if !ok || src != "vault" || got != "sk-test-value" {
		t.Errorf("stored key = (%q, %q, %v)", got, src, ok)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if raw, _ := tx.Get(vault.Secrets, "OPENAI_API_KEY"); strings.Contains(raw, "sk-test-value") {
			t.Error("vault record contains the plaintext secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "logged in") {
		t.Error("login must not have run after being declined")
	}
	for _, want := range []string{"Browser login — openai login? [Y/n]", "Paste API key — create at platform.openai.com/api-keys"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dialog lacks %q:\n%s", want, out.String())
		}
	}
}

// --check on a present, rotatable credential (codex's ChatGPT login) must stay read-only: no "keep it?"
// prompt, no rotate CLI invoked, no file write — the new rotate feature must not change --check's contract.
func TestCheckStaysReadOnlyWithRotatableCredential(t *testing.T) {
	cfg, home, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("HARNESS_PROVIDER", "subscription")
	t.Setenv("PATH", t.TempDir()) // no codex binary: forces the auth.json file check, never execs a CLI
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Vendor: "codex", Home: home, In: strings.NewReader("")})
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

// "claude" is a Sections entry: present in the walk order, and listed in the unknown-section error so
// a typo'd section name tells the user it is a valid choice.
func TestClaudeSectionIsRegistered(t *testing.T) {
	if !slices.Contains(Sections, "claude") {
		t.Fatalf("Sections = %v, want \"claude\" present", Sections)
	}
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "bogus-section")
	if rc != 2 {
		t.Errorf("rc = %d, want 2:\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "claude") {
		t.Errorf("unknown-section output lacks %q:\n%s", "claude", out.String())
	}
}

// The Max-plan row offers three ways in: browser login, minting a headless token, pasting one; the minted token
// is recognised in the CLI's output by its sk-ant-oat prefix.
func TestMaxPlanOptionsAndSetupTokenCapture(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude binary: status reads as not logged in
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	kt := maxPlanTarget(keyTarget{label: "claude"})
	if kt.present || kt.headless {
		t.Fatalf("no login and no token must read absent: %+v", kt)
	}
	opts := authOptions(kt)
	if len(opts) != 3 || opts[0].argv == nil || opts[1].mint == nil || opts[1].env != "CLAUDE_CODE_OAUTH_TOKEN" || opts[2].env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("authOptions = %+v, want login, mint, paste", opts)
	}
	out := "Opening browser...\nPaste the code here:\n\nYour token: sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789\n"
	if got := setupTokenRE.FindString(out); got != "sk-ant-oat01-abcDEF_ghi-JKLmno_pqrSTUvwx-YZ0123456789" {
		t.Errorf("token capture = %q", got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-stored")
	if kt := maxPlanTarget(keyTarget{label: "claude"}); !kt.present || !kt.headless || !strings.Contains(kt.source, "headless token") {
		t.Errorf("a stored token must read present/headless: %+v", kt)
	}
}
