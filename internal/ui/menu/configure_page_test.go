package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row the menu tests apply to
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// servedHarness seeds a hermetic vault with a Databricks row "qa" (served-aware routing set to on) pinned as the
// session profile, and a TLS listing server; it returns the request counter.
func servedHarness(t *testing.T, on bool) *int {
	t.Helper()
	vaulttest.Use(t)
	var n int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"m-a","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, srv.URL))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "dapi-qa")
	return &n
}

func cached(t *testing.T) bool {
	t.Helper()
	_, ok := router.LoadServed("qa")
	return ok
}

// Run wires the warm: a non-interactive provider pin change with the switch on warms; --dry-run does not.
func TestRunProviderPinChangeWarmsUnlessDryRun(t *testing.T) {
	n := servedHarness(t, true)
	var out bytes.Buffer
	if rc := Run(Options{Provider: "qa", DryRun: true, In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 || *n != 0 {
		t.Fatalf("dry-run: rc=%d requests=%d out=%s", rc, *n, out.String())
	}
	Run(Options{Provider: "qa", Check: true, In: strings.NewReader(""), Out: &out}, "provider")
	if *n != 0 {
		t.Fatalf("--check warmed: %d requests", *n)
	}
	if rc := Run(Options{Provider: "qa", In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, out.String())
	}
	if vault.GetAgentRecord("claude").Profile != "qa" || *n == 0 || !cached(t) {
		t.Fatalf("pin=%q requests=%d cached=%v out=%s", vault.GetAgentRecord("claude").Profile, *n, cached(t), out.String())
	}
}
