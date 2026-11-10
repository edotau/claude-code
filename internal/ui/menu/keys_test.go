package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestKeyRows: one row per credential, absent ones say how to fix, and allOK tracks a missing credential.
func TestKeyRows(t *testing.T) {
	rows, ok := keyRows([]configure.KeyTarget{
		{Label: "codex", Present: true, Source: "ChatGPT login (codex login)"},
		{Label: "gemini", Present: false},
	})
	if len(rows) != 2 || rows[0][1] != "subscription  verified" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[1][1] != "not set up    not verified" || ok {
		t.Fatalf("absent row = %q ok=%v, want the set-up hint and ok=false", rows[1][1], ok)
	}
}

// TestKeysMenuBackChangesNothing: leaving the menu at once runs no action and reports nothing changed.
func TestKeysMenuBackChangesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no real vendor CLIs: building key rows must not exec opencode/codex/gh
	orig := pickKey
	t.Cleanup(func() { pickKey = orig })
	calls := 0
	pickKey = func(string, [][2]string, int) (int, error) { calls++; return -1, nil }
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	if _, changed := keysMenu(&dialog{reg: reg}); changed || calls != 1 {
		t.Fatalf("changed=%v calls=%d, want unchanged after one Back", changed, calls)
	}
}

// TestLoginLine: every login row's mode is "subscription", with the tier when the CLI reports one.
func TestLoginLine(t *testing.T) {
	cases := []struct {
		kt   configure.KeyTarget
		want string
	}{
		{configure.KeyTarget{Label: "claude", Source: "claude.ai login (max)", Headless: true}, "subscription (max)"},
		{configure.KeyTarget{Label: "codex", Source: "ChatGPT login (codex login)"}, "subscription"},
		{configure.KeyTarget{Label: "opencode", Source: "opencode auth login: github-copilot"}, "subscription"},
		{configure.KeyTarget{Label: "copilot", Source: "GH_TOKEN / gh auth status"}, "subscription"},
	}
	for _, c := range cases {
		if got := loginLine(c.kt); got != c.want {
			t.Errorf("loginLine(%s) = %q, want %q", c.kt.Label, got, c.want)
		}
	}
}

// Given the Max-plan login (AuthCLI probes as "login"), the row reads subscription (tier) and verified.
func TestKeyColsMaxPlanLoginIsVerified(t *testing.T) {
	kt := configure.KeyTarget{Label: "claude", Present: true, EnvVar: "X", Source: "claude.ai login (max)",
		Provider: &providers.Provider{Auth: providers.Auth{Type: providers.AuthCLI}}}
	if got := keyCols(kt); got != [2]string{"subscription (max)", "verified"} {
		t.Errorf("keyCols = %v, want subscription (max) / verified", got)
	}
}

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
		Name: "fake", Protocol: providers.ProtocolAnthropic, BaseURL: srv.URL,
		Auth: providers.Auth{Type: providers.AuthKey, Header: providers.HeaderXAPIKey, Env: envVar},
	}
}

func newDialog(in string) (*dialog, *bytes.Buffer) {
	r := strings.NewReader(in)
	var out bytes.Buffer
	return &dialog{o: Options{Interactive: true, In: r, Out: &out}, br: bufio.NewReader(r), out: &out}, &out
}

// --- rotateOptions ---

// --- runRotate ---

func TestRunRotateNoOptionsFalse(t *testing.T) {
	d, _ := newDialog("")
	if runRotate(d, configure.KeyTarget{}) {
		t.Error("runRotate with no options must be false")
	}
}

// A single login option runs unconditionally (no confirm to pick among one); a missing binary fails fast
// (LookPath), proving the wiring reaches runLogin without needing a real CLI or hanging.
func TestRunRotateSingleLoginOptionNoBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("")
	kt := configure.KeyTarget{Label: "codex", LoginArgv: []string{"codex-does-not-exist", "login"}}
	if runRotate(d, kt) {
		t.Errorf("runRotate must fail when the login CLI is missing:\n%s", out.String())
	}
}

// A single mint option runs unconditionally; the printed token lands in the vault.
func TestRunRotateSingleMintOptionStoresVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	if err := vault.Store(providers.OAuthTokenEnv, "old-token-value"); err != nil {
		t.Fatal(err)
	}
	d, out := newDialog("")
	kt := configure.KeyTarget{Label: "claude", EnvVar: providers.OAuthTokenEnv, MintArgv: []string{"/bin/echo", "sk-ant-oat1-newtokenabcDEFghijklmno0123456789"}}
	if !runRotate(d, kt) {
		t.Fatalf("runRotate(mint) = false:\n%s", out.String())
	}
	val, src, ok := vault.Get(providers.OAuthTokenEnv)
	if !ok || src != "vault" || !strings.Contains(val, "newtokenabc") {
		t.Errorf("vault.Get after mint = (%q, %q, %v), want the new token in the vault", val, src, ok)
	}
}

// Two options, piped: declining the first option's confirm picks the second.
func TestRunRotateTwoOptionsPipedPicksSecondOnDecline(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("n\n")
	kt := configure.KeyTarget{Label: "claude", LoginArgv: []string{"missing-login-cli"}, MintArgv: []string{"missing-mint-cli"}}
	runRotate(d, kt) // exercising which branch ran, not the (missing-binary) result
	if !strings.Contains(out.String(), "Login") {
		t.Errorf("expected the first option's confirm prompt in output:\n%s", out.String())
	}
}

// --- regenerate ---

// Owner decision (2026-11-03, keeps 22f1342c): an env-only mint is saved to the vault with an override warning.
func TestRegenerateMintEnvOnlySavesToVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROT_MINT_ENV", "set-via-process-env")
	d, out := newDialog("")
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_MINT_ENV"}
	const minted = "sk-ant-oat1-newvaluevalue0123456789abcdef"
	if !regenerate(d, kt, []string{"/bin/echo", minted}, true) {
		t.Fatalf("regenerate(mint) on an env-only credential must save the new token:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "still overrides") {
		t.Errorf("output must warn that the exported var still overrides the vault:\n%s", out.String())
	}
	if v := os.Getenv("ROT_MINT_ENV"); v != "set-via-process-env" {
		t.Errorf("the exported credential must be left untouched, got %q", v)
	}
	os.Unsetenv("ROT_MINT_ENV") // t.Setenv restores it; now the vault copy is visible
	if v, src, _ := vault.Get("ROT_MINT_ENV"); v != minted || src == "env" {
		t.Errorf("vault must hold the minted token: got %q from %q", v, src)
	}
}

func TestRegenerateLoginFailurePrintsError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, out := newDialog("")
	if regenerate(d, configure.KeyTarget{Label: "codex"}, []string{"codex-does-not-exist"}, false) {
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
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_REPLACE_ENV"}
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
	if err := vault.Store("ROT_REPLACE_KEY", oldVal); err != nil {
		t.Fatal(err)
	}
	p := fakeKeyedProvider(t, "ROT_REPLACE_KEY", "the-only-accepted-value")
	const newVal = "brand-new-but-rejected-value-0002"
	d, out := newDialog(newVal + "\ny\n") // paste, then "y" to the keep-old prompt
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_REPLACE_KEY", Provider: p}
	if replaceKey(d, kt) {
		t.Fatalf("replaceKey must report false when the old value is kept:\n%s", out.String())
	}
	val, src, ok := vault.Get("ROT_REPLACE_KEY")
	if !ok || src != "vault" || val != oldVal {
		t.Errorf("vault.Get = (%q, %q, %v), want the old value untouched", val, src, ok)
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
	if err := vault.Store("ROT_REPLACE_OK", "old-value-irrelevant-here"); err != nil {
		t.Fatal(err)
	}
	const newVal = "accepted-fresh-secret-value-99"
	p := fakeKeyedProvider(t, "ROT_REPLACE_OK", newVal)
	d, out := newDialog(newVal + "\n")
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_REPLACE_OK", Provider: p}
	if !replaceKey(d, kt) {
		t.Fatalf("replaceKey must succeed on an accepted probe:\n%s", out.String())
	}
	val, src, ok := vault.Get("ROT_REPLACE_OK")
	if !ok || src != "vault" || val != newVal {
		t.Errorf("vault.Get = (%q, %q, %v), want the new value in the vault", val, src, ok)
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
	kt := configure.KeyTarget{Label: "x"}
	if !ensureKey(d, kt, true) {
		t.Fatalf("ensureKey must keep the present key on confirm:\n%s", out.String())
	}
}

func TestEnsureKeyPresentDeclineRunsReplace(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROT_ENSURE_ENV", "env-only-so-replace-must-refuse")
	d, out := newDialog("n\n")
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_ENSURE_ENV"}
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
	kt := configure.KeyTarget{Label: "x", EnvVar: "ROT_ENSURE_ABSENT", FixCmd: "x"}
	if !ensureKey(d, kt, false) {
		t.Fatalf("ensureKey on an absent key must run resolveKey:\n%s", out.String())
	}
	val, _, ok := vault.Get("ROT_ENSURE_ABSENT")
	if !ok || val != "pasted-value-here" {
		t.Errorf("resolveKey must have stored the pasted value, got (%q, %v)", val, ok)
	}
}

// Adding a custom secret seals it in the vault and shows only its redacted value.
func TestSecretsAddCustomStoresVaultAndRedacts(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	newRow := len(configure.TrackedSecrets(reg)) + 1 // "+ new" is the row right after every tracked secret
	in := strings.NewReader(fmt.Sprintf("%d\nMY_NEW_SECRET\nsk-super-secret-raw-value\n\n", newRow))
	var out bytes.Buffer
	rc := Secrets(Options{In: in, Out: &out})
	if rc != 0 {
		t.Fatalf("Secrets rc = %d:\n%s", rc, out.String())
	}
	if value, src, ok := vault.Get("MY_NEW_SECRET"); !ok || src != "vault" || value != "sk-super-secret-raw-value" {
		t.Fatalf("stored secret = (%q, %q, %v)", value, src, ok)
	}
	transcript := out.String()
	if strings.Contains(transcript, "sk-super-secret-raw-value") {
		t.Fatalf("transcript leaked the raw value:\n%s", transcript)
	}
	if !strings.Contains(transcript, "MY_NEW_SECRET") {
		t.Errorf("transcript does not name MY_NEW_SECRET:\n%s", transcript)
	}
}

// storeValue refuses a Tailscale credential that doesn't have the documented shape and leaves the
// vault untouched.
func TestStoreValueRefusesMalformedTailscaleCredential(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	d := &dialog{out: &out}
	if storeValue(d, "TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-auth-wrongkind") {
		t.Fatalf("storeValue(...) = true for a secret shaped like an auth key, want refused:\n%s", out.String())
	}
	if _, _, ok := vault.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok {
		t.Fatal("a refused credential must not be stored")
	}
	if !strings.Contains(out.String(), "tskey-client-") {
		t.Errorf("output = %q, want it to name the expected tskey-client- prefix", out.String())
	}
}

// Re-picking a secret name to paste a new value after staging its delete must cancel the staged
// delete: pendingDeletes must drop the name, and the new value must land in pending.
func TestStoreValueCancelsStagedDelete(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_RESTAGE_AFTER_DELETE"
	var out bytes.Buffer
	d := &dialog{out: &out, pending: map[string]stagedKey{}, pendingDeletes: map[string]bool{name: true}}

	if !storeValue(d, name, "newval") {
		t.Fatalf("storeValue(...) = false:\n%s", out.String())
	}
	if d.pendingDeletes[name] {
		t.Error("pendingDeletes must no longer carry the name once it is re-staged with a new value")
	}
	if got := d.pending[name]; got.val != "newval" {
		t.Errorf("pending[%q].val = %q, want %q", name, got.val, "newval")
	}
}

func TestSourceLabel(t *testing.T) {
	cases := []struct{ src, shadowed, want string }{
		{"", "", "missing"}, {"vault", "", "vault"}, {"env", "", "env"},
		{"env", "vault", "env over vault"}, {"unreadable", "", "vault unreadable"},
	}
	for _, c := range cases {
		if got := sourceLabel(c.src, c.shadowed); got != c.want {
			t.Errorf("sourceLabel(%q, %q) = %q, want %q", c.src, c.shadowed, got, c.want)
		}
	}
}

// The secrets page secretsLoop draws, the setup-token shadowed (env over vault) and a custom secret, fits 80 columns.
func TestSecretsRowWidthFitsEightyColumns(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	vaulttest.Secret(t, providers.OAuthTokenEnv, "sk-ant-oat01-vault")
	vaulttest.Secret(t, "A_CUSTOM", "v")
	t.Setenv(providers.OAuthTokenEnv, "sk-ant-oat01-env")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	d, out := newDialog("")
	d.reg = reg
	secretsLoop(d)
	rows := numberedRows(out.String())
	if !strings.Contains(out.String(), "env over vault") {
		t.Fatalf("no shadowed setup-token row in:\n%s", out.String())
	}
	assertFitsEighty(t, "secrets page", rows)
}

func TestEditSecretDeletesVaultValue(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_DELETE_SECRET"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	d, out := newDialog("2\ny\n")
	editSecret(d, configure.TrackedSecret{Name: name, Source: "vault"})
	if _, src, ok := vault.Get(name); ok || src != "" {
		t.Fatalf("deleted secret still resolves from %q", src)
	}
	if !strings.Contains(out.String(), "deleted "+name) {
		t.Errorf("missing deletion confirmation: %s", out.String())
	}
}

// TestEditSecretDeleteStagedDropsStagedStoreAndIsAuditedOnCommit: inside the menu (d.pending set),
// deleting a tracked secret that already has a staged Store of the same name drops that staged
// Store (restoring the env it shadowed) instead of deleting the vault immediately; the vault delete
// — and its audit entry — only happens when the page's staged changes are later committed.
func TestEditSecretDeleteStagedDropsStagedStoreAndIsAuditedOnCommit(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_STAGED_DELETE"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	since := time.Now().Add(-time.Second)

	d, out := newDialog("2\ny\n") // pick "Delete" (row 2: Paste API key, Delete), then confirm
	d.pending = map[string]stagedKey{}
	if !d.stage(name, "replacement-value") {
		t.Fatal("stage failed")
	}
	editSecret(d, configure.TrackedSecret{Name: name, Source: "vault"})

	if !d.pendingDeletes[name] {
		t.Error("the delete must be staged in pendingDeletes")
	}
	if !strings.Contains(out.String(), "staged delete of "+name) {
		t.Errorf("output = %q, want a staged-delete confirmation", out.String())
	}
	if v, _, ok := vault.Get(name); !ok || v != "stored-value" {
		t.Fatalf("vault value changed before commit: (%q, %v), want still stored-value", v, ok)
	}
	assertNoSecretDeleteAudit(t, since)

	// Commit the page's staged changes, as secretsLoop's defer would on leaving the page.
	confirmPageKeys(d, map[string]stagedKey{})
	if _, staged := d.pending[name]; staged {
		t.Error("the staged Store must be dropped once the delete commits")
	}
	if got := os.Getenv(name); got != "" {
		t.Errorf("env after the delete commits = %q, want restored to empty", got)
	}
	if _, _, ok := vault.Get(name); ok {
		t.Error("the vault value should be deleted once the staged delete commits")
	}
	assertSecretDeleteAudited(t, since, name)
}

// assertNoSecretDeleteAudit requires no secret_delete audit entry since the given time.
func assertNoSecretDeleteAudit(t *testing.T, since time.Time) {
	t.Helper()
	events, err := audit.Read(since)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete {
			t.Fatalf("secret_delete audited before the staged delete was committed: %+v", e)
		}
	}
}

// assertSecretDeleteAudited requires a secret_delete audit entry for name since the given time.
func assertSecretDeleteAudited(t *testing.T, since time.Time, name string) {
	t.Helper()
	events, err := audit.Read(since)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete && e.Resource == name {
			return
		}
	}
	t.Errorf("commit did not audit secret_delete for %s: %+v", name, events)
}

func TestCustomEnvPlainAndSecret(t *testing.T) {
	for _, kind := range []string{"plain", "secret"} {
		t.Run(kind, func(t *testing.T) {
			vaulttest.UseKeyring(t)
			const name = "CONFIGURE_CUSTOM_ENV"
			t.Setenv(name, "")
			choice := "1"
			want := envEdit{raw: "custom-value"}
			if kind == "secret" {
				choice = "2"
				want = envEdit{raw: providers.SecretRef, secret: "custom-value"}
			}
			d, out := newDialog("custom-value\n" + choice + "\n")
			gotName, edit, ok := editCustomEnv(d, name)
			if !ok || gotName != name || edit != want {
				t.Fatalf("edit = (%q, %+v, %v), want %q, %+v", gotName, edit, ok, name, want)
			}
			orig := saveConfirmed
			t.Cleanup(func() { saveConfirmed = orig })
			saveConfirmed = func(string) bool { return true }
			saveEnvEdits(d, map[string]envEdit{name: edit})
			if got := providers.CustomEnv()[name]; got != want.raw {
				t.Errorf("vault env = %q, want %q", got, want.raw)
			}
			assertCustomEnvSecret(t, name, kind, want.secret)
			if !sameAsSaved(name, edit) || strings.Contains(out.String(), "custom-value") {
				t.Errorf("saved edit differs or transcript leaks value: %s", out.String())
			}
		})
	}
}

// assertCustomEnvSecret requires a "secret" custom env to be sealed in the vault with want, and a "plain" one not.
func assertCustomEnvSecret(t *testing.T, name, kind, want string) {
	t.Helper()
	value, src, found := vault.Get(name)
	if kind == "secret" && (!found || src != "vault" || value != want) {
		t.Errorf("secret = (%q, %q, %v)", value, src, found)
	}
	if kind == "plain" && found {
		t.Error("plain env value also stored as a secret")
	}
}

// TestEditCustomEnvRejectsInvalidName: a new custom-env NAME must pass the same identifier shape
// every other secret name does; a bad name is refused before any value is read or stored.
func TestEditCustomEnvRejectsInvalidName(t *testing.T) {
	vaulttest.UseKeyring(t)
	d, out := newDialog("not a valid name!\n")
	name, edit, ok := editCustomEnv(d, "")
	if ok {
		t.Fatalf("editCustomEnv accepted an invalid NAME: name=%q edit=%+v", name, edit)
	}
	if !strings.Contains(out.String(), "not a valid name") {
		t.Errorf("output = %q, want a rejection message naming the shape rule", out.String())
	}
}

// TestEditCustomEnvDeletingSecretVarDeletesSealedSecretAfterConfirm: clearing an @secret custom-env
// var offers to delete the sealed secret it pointed at; confirming (y) removes both the vault env
// entry and the sealed secret, declining (n) leaves the sealed secret alone.
func TestEditCustomEnvDeletingSecretVarDeletesSealedSecretAfterConfirm(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		wantDeleted  bool
	}{
		{"CONFIGURE_ENV_SECRET_DEL_YES", "y\n", true},
		{"CONFIGURE_ENV_SECRET_DEL_NO", "n\n", false},
	} {
		t.Run(tc.answer, func(t *testing.T) {
			vaulttest.UseKeyring(t)
			if err := providers.SetCustomEnv(tc.name, providers.SecretRef); err != nil {
				t.Fatal(err)
			}
			if err := vault.Store(tc.name, "sealed-value"); err != nil {
				t.Fatal(err)
			}
			d, _ := newDialog("\n" + tc.answer) // empty value line (delete), then the confirm answer
			gotName, edit, ok := editCustomEnv(d, tc.name)
			if !ok || gotName != tc.name || edit.raw != "" || edit.deleteSecret != tc.wantDeleted {
				t.Fatalf("edit = (%q, %+v, %v), want raw=\"\" deleteSecret=%v", gotName, edit, ok, tc.wantDeleted)
			}
			orig := saveConfirmed
			t.Cleanup(func() { saveConfirmed = orig })
			saveConfirmed = func(string) bool { return true }
			saveEnvEdits(d, map[string]envEdit{tc.name: edit})
			_, _, found := vault.Get(tc.name)
			if found == tc.wantDeleted {
				t.Errorf("sealed secret found=%v, want found=%v", found, !tc.wantDeleted)
			}
			if _, ok := providers.CustomEnv()[tc.name]; ok {
				t.Error("vault env entry should have been removed either way")
			}
		})
	}
}

// A key staged before the secrets page opened, then deleted on the page and discarded at the leave prompt,
// must keep its earlier staged value (and env): the cancel only undoes what the page itself changed.
func TestStagedDeleteCancelKeepsPrePageStagedEdit(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_PREPAGE_STAGED"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	orig := saveConfirmed
	saveConfirmed = func(string) bool { return false }
	t.Cleanup(func() { saveConfirmed = orig })

	d, _ := newDialog("y\n")
	d.pending = map[string]stagedKey{}
	if !d.stage(name, "pre-page-edit") {
		t.Fatal("stage failed")
	}
	before := maps.Clone(d.pending)
	if !deleteSecret(d, name) {
		t.Fatal("deleteSecret = false")
	}
	confirmPageKeys(d, before)

	if got := d.pending[name]; got.val != "pre-page-edit" {
		t.Errorf("pending[%q] = %+v after cancel, want the pre-page staged edit kept", name, got)
	}
	if got := os.Getenv(name); got != "pre-page-edit" {
		t.Errorf("env = %q after cancel, want pre-page-edit", got)
	}
	if d.pendingDeletes[name] {
		t.Error("cancel must clear the staged delete")
	}
}

// A key both staged on the page and staged for delete is one logical change, not two, in the discard message.
func TestConfirmPageKeysCountsStagedAndDeletedKeyOnce(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_COUNT_ONCE"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	orig := saveConfirmed
	saveConfirmed = func(string) bool { return false }
	t.Cleanup(func() { saveConfirmed = orig })

	d, out := newDialog("y\n")
	d.pending = map[string]stagedKey{}
	before := maps.Clone(d.pending)
	if !d.stage(name, "page-value") {
		t.Fatal("stage failed")
	}
	if !deleteSecret(d, name) {
		t.Fatal("deleteSecret = false")
	}
	confirmPageKeys(d, before)

	if !strings.Contains(out.String(), "secrets: 1 change(s) discarded") {
		t.Errorf("output = %q, want 1 change(s) discarded", out.String())
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
