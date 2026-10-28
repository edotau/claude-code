package configure

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeKeyedProvider is a provider whose catalog route is a local server; want is the only candidate the
// server accepts, so probeValue/ProbeCredential drive "verified" vs "rejected" without a real network call.
func fakeKeyedProvider(t *testing.T, envVar, want string) *providers.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return &providers.Provider{
		Name: "fake", Kind: providers.DialectAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar},
	}
}

func newDialog(in string) (*dialog, *bytes.Buffer) {
	r := strings.NewReader(in)
	var out bytes.Buffer
	return &dialog{o: Options{Interactive: true, In: r, Out: &out}, br: bufio.NewReader(r), out: &out}, &out
}

// --- rotateOptions ---

func TestRotateOptionsShapes(t *testing.T) {
	cases := []struct {
		name string
		kt   keyTarget
		want []string
	}{
		{"login only", keyTarget{loginArgv: []string{"codex", "login"}}, []string{"Login"}},
		{"mint only", keyTarget{mintArgv: []string{"claude", "setup-token"}}, []string{"OAuth token"}},
		{"login and mint (subscription)", keyTarget{loginArgv: []string{"claude", "auth", "login"}, mintArgv: []string{"claude", "setup-token"}},
			[]string{"Login", "OAuth token"}},
		{"keyed, no generator", keyTarget{envVar: "X", provider: &providers.Provider{}}, []string{"API key"}},
		{"keyed but no provider: no generator and no paste", keyTarget{envVar: "X"}, nil},
		{"nothing at all", keyTarget{}, nil},
	}
	for _, c := range cases {
		opts := rotateOptions(c.kt)
		var got []string
		for _, o := range opts {
			got = append(got, o.action)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: rotateOptions = %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: rotateOptions = %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

// --- runRotate ---

func TestRunRotateNoOptionsFalse(t *testing.T) {
	d, _ := newDialog("")
	if runRotate(d, keyTarget{}) {
		t.Error("runRotate with no options must be false")
	}
}

// A single login option runs unconditionally (no confirm to pick among one); a missing binary fails fast
// (LookPath), proving the wiring reaches runLogin without needing a real CLI or hanging.
func TestRunRotateSingleLoginOptionNoBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("")
	kt := keyTarget{label: "codex", loginArgv: []string{"codex-does-not-exist", "login"}}
	if runRotate(d, kt) {
		t.Errorf("runRotate must fail when the login CLI is missing:\n%s", out.String())
	}
}

// A single mint option runs unconditionally; the printed token lands in the vault.
func TestRunRotateSingleMintOptionStoresVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	if err := secrets.Store(providers.OAuthTokenEnv, "old-token-value"); err != nil {
		t.Fatal(err)
	}
	d, out := newDialog("")
	kt := keyTarget{label: "claude", envVar: providers.OAuthTokenEnv, mintArgv: []string{"/bin/echo", "sk-ant-oat1-newtokenabcDEFghijklmno0123456789"}}
	if !runRotate(d, kt) {
		t.Fatalf("runRotate(mint) = false:\n%s", out.String())
	}
	val, src, ok := secrets.Get(providers.OAuthTokenEnv)
	if !ok || src != "vault" || !strings.Contains(val, "newtokenabc") {
		t.Errorf("secrets.Get after mint = (%q, %q, %v), want the new token in the vault", val, src, ok)
	}
}

// Two options, piped: declining the first option's confirm picks the second.
func TestRunRotateTwoOptionsPipedPicksSecondOnDecline(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("n\n")
	kt := keyTarget{label: "claude", loginArgv: []string{"missing-login-cli"}, mintArgv: []string{"missing-mint-cli"}}
	runRotate(d, kt) // exercising which branch ran, not the (missing-binary) result
	if !strings.Contains(out.String(), "Login") {
		t.Errorf("expected the first option's confirm prompt in output:\n%s", out.String())
	}
}

// --- regenerate ---

func TestRegenerateMintEnvOnlyRefuses(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROT_MINT_ENV", "set-via-process-env")
	d, out := newDialog("")
	kt := keyTarget{label: "x", envVar: "ROT_MINT_ENV"}
	if regenerate(d, kt, []string{"/bin/echo", "sk-ant-oat1-newvaluevalue0123456789abcdef"}, true) {
		t.Fatal("regenerate(mint) on an env-only credential must refuse")
	}
	if !strings.Contains(out.String(), "env-only") {
		t.Errorf("output must explain the env-only refusal:\n%s", out.String())
	}
	if v := os.Getenv("ROT_MINT_ENV"); v != "set-via-process-env" {
		t.Errorf("env-only credential must be left untouched, got %q", v)
	}
}

func TestRegenerateLoginFailurePrintsError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("")
	if regenerate(d, keyTarget{label: "codex"}, []string{"codex-does-not-exist"}, false) {
		t.Fatal("regenerate(login) must fail when the CLI is missing")
	}
	if !strings.Contains(out.String(), "codex") {
		t.Errorf("output must name the target on failure:\n%s", out.String())
	}
}

// --- replaceKey ---

func TestReplaceKeyEnvOnlyRefuses(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROT_REPLACE_ENV", "set-via-process-env")
	d, out := newDialog("new-pasted-value\n")
	kt := keyTarget{label: "x", envVar: "ROT_REPLACE_ENV"}
	if replaceKey(d, kt) {
		t.Fatal("replaceKey on an env-only credential must refuse")
	}
	if !strings.Contains(out.String(), "env-only") {
		t.Errorf("output must explain the refusal:\n%s", out.String())
	}
}

// A rejected probe asks to keep the old value; answering yes keeps it, writes nothing, and never echoes
// either the old or the new raw value — only a 5-char-prefix redaction ever appears.
func TestReplaceKeyProbeRejectedKeepsOldOnConfirm(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	const oldVal = "old-good-secret-value-0001"
	if err := secrets.Store("ROT_REPLACE_KEY", oldVal); err != nil {
		t.Fatal(err)
	}
	p := fakeKeyedProvider(t, "ROT_REPLACE_KEY", "the-only-accepted-value")
	const newVal = "brand-new-but-rejected-value-0002"
	d, out := newDialog(newVal + "\ny\n") // paste, then "y" to the keep-old prompt
	kt := keyTarget{label: "x", envVar: "ROT_REPLACE_KEY", provider: p}
	if replaceKey(d, kt) {
		t.Fatalf("replaceKey must report false when the old value is kept:\n%s", out.String())
	}
	val, src, ok := secrets.Get("ROT_REPLACE_KEY")
	if !ok || src != "vault" || val != oldVal {
		t.Errorf("secrets.Get = (%q, %q, %v), want the old value untouched", val, src, ok)
	}
	for _, raw := range []string{oldVal, newVal} {
		if strings.Contains(out.String(), raw) {
			t.Fatalf("transcript leaked a raw secret %q:\n%s", raw, out.String())
		}
	}
}

// An accepted probe stores the new value back to the vault, and the transcript shows
// only its 5-char-prefix redaction, never the full value.
func TestReplaceKeyProbeAcceptedStoresVaultRedacted(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	if err := secrets.Store("ROT_REPLACE_OK", "old-value-irrelevant-here"); err != nil {
		t.Fatal(err)
	}
	const newVal = "accepted-fresh-secret-value-99"
	p := fakeKeyedProvider(t, "ROT_REPLACE_OK", newVal)
	d, out := newDialog(newVal + "\n")
	kt := keyTarget{label: "x", envVar: "ROT_REPLACE_OK", provider: p}
	if !replaceKey(d, kt) {
		t.Fatalf("replaceKey must succeed on an accepted probe:\n%s", out.String())
	}
	val, src, ok := secrets.Get("ROT_REPLACE_OK")
	if !ok || src != "vault" || val != newVal {
		t.Errorf("secrets.Get = (%q, %q, %v), want the new value in the vault", val, src, ok)
	}
	if strings.Contains(out.String(), newVal) {
		t.Fatalf("transcript leaked the raw new value:\n%s", out.String())
	}
	if !strings.Contains(out.String(), newVal[:5]) {
		t.Errorf("transcript must show the 5-char prefix redaction:\n%s", out.String())
	}
}

// --- ensureKey ---

func TestEnsureKeyPresentKeepsOnConfirm(t *testing.T) {
	d, out := newDialog("y\n")
	kt := keyTarget{label: "x"}
	if !ensureKey(d, kt, true) {
		t.Fatalf("ensureKey must keep the present key on confirm:\n%s", out.String())
	}
}

func TestEnsureKeyPresentDeclineRunsReplace(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROT_ENSURE_ENV", "env-only-so-replace-must-refuse")
	d, out := newDialog("n\n")
	kt := keyTarget{label: "x", envVar: "ROT_ENSURE_ENV"}
	if ensureKey(d, kt, true) {
		t.Fatal("ensureKey declining keep must fall to replaceKey, which refuses an env-only credential")
	}
	if !strings.Contains(out.String(), "env-only") {
		t.Errorf("replaceKey's env-only refusal must surface through ensureKey:\n%s", out.String())
	}
}

func TestEnsureKeyAbsentRunsResolve(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("y\npasted-value-here\n")
	kt := keyTarget{label: "x", envVar: "ROT_ENSURE_ABSENT", fixCmd: "x"}
	if !ensureKey(d, kt, false) {
		t.Fatalf("ensureKey on an absent key must run resolveKey:\n%s", out.String())
	}
	val, _, ok := secrets.Get("ROT_ENSURE_ABSENT")
	if !ok || val != "pasted-value-here" {
		t.Errorf("resolveKey must have stored the pasted value, got (%q, %v)", val, ok)
	}
}
