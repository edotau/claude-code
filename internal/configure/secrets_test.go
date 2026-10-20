package configure

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestParseAssignment(t *testing.T) {
	cases := []struct {
		raw, name, value string
	}{
		{"sk-bare-value", "", "sk-bare-value"},
		{"NAME=v", "NAME", "v"},
		{`export NAME="v"`, "NAME", "v"},
		{"NAME='v'", "NAME", "v"},
		{"abc==", "", "abc=="},   // base64 padding stays a bare value, never split as NAME=value
		{"ABC=", "", "ABC="},     // trailing '=' with no value: Cut's v=="" rule keeps it bare
		{"name=v", "", "name=v"}, // lowercase "key" is not an env-var-shaped name: stays bare
	}
	for _, c := range cases {
		name, value := ParseAssignment(c.raw)
		if name != c.name || value != c.value {
			t.Errorf("ParseAssignment(%q) = (%q, %q), want (%q, %q)", c.raw, name, value, c.name, c.value)
		}
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
	newRow := len(trackedSecrets(reg)) + 1 // "+ new" is the row right after every tracked secret
	in := strings.NewReader(fmt.Sprintf("%d\nMY_NEW_SECRET\nsk-super-secret-raw-value\n\n", newRow))
	var out bytes.Buffer
	rc := Secrets(Options{In: in, Out: &out})
	if rc != 0 {
		t.Fatalf("Secrets rc = %d:\n%s", rc, out.String())
	}
	if value, src, ok := secrets.Get("MY_NEW_SECRET"); !ok || src != "vault" || value != "sk-super-secret-raw-value" {
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
	if _, _, ok := secrets.Get("TAILSCALE_OAUTH_CLIENT_SECRET"); ok {
		t.Fatal("a refused credential must not be stored")
	}
	if !strings.Contains(out.String(), "tskey-client-") {
		t.Errorf("output = %q, want it to name the expected tskey-client- prefix", out.String())
	}
}

func TestSourceLabel(t *testing.T) {
	cases := map[string]string{"": "missing", "vault": "vault", "env": "env"}
	for src, want := range cases {
		if got := sourceLabel(src, ""); got != want {
			t.Errorf("sourceLabel(%q, \"\") = %q, want %q", src, got, want)
		}
	}
	shadowCases := []struct{ src, shadowed, want string }{
		{"env", "vault", "env over vault"},
	}
	for _, c := range shadowCases {
		if got := sourceLabel(c.src, c.shadowed); got != c.want {
			t.Errorf("sourceLabel(%q, %q) = %q, want %q", c.src, c.shadowed, got, c.want)
		}
	}
}

// Every secrets-page row, worst case "env over vault" shadowing the setup-token's long name and
// user list, must still render at 80 columns or fewer (mirrors rowRenderedWidth in rowwidth_test.go).
func TestSecretsRowWidthFitsEightyColumns(t *testing.T) {
	list := []trackedSecret{
		{name: providers.OAuthTokenEnv, source: "env", shadowed: "vault", users: "claude -p (setup-token)"},
		{name: "A_CUSTOM", source: "vault", shadowed: "", users: "custom"},
	}
	var labels []string
	for _, s := range list {
		labels = append(labels, s.name)
	}
	for _, s := range list {
		detail := fmt.Sprintf("%-16s %s", sourceLabel(s.source, s.shadowed), s.users)
		if w := rowRenderedWidth(labels, detail); w > 80 {
			t.Errorf("secrets row %q = %d cols, want <=80: %q", s.name, w, detail)
		}
	}
}

// The list is harness-read names plus the user's own: a console-URL key no enabled provider reads is not a row,
// an indexed custom secret is.
func TestTrackedSecretsListsHarnessAndCustomOnly(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("PATH", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers": {"openai": null}}`)
	if err := secrets.Store("MY_CUSTOM", "v"); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, s := range trackedSecrets(reg) {
		rows[s.name] = s.users
	}
	if _, ok := rows["OPENAI_API_KEY"]; ok {
		t.Error("OPENAI_API_KEY listed although its provider is disabled")
	}
	if rows["MY_CUSTOM"] != "custom" || rows[providers.OAuthTokenEnv] == "" {
		t.Errorf("rows = %v, want MY_CUSTOM (custom) and the setup-token", rows)
	}
}

func TestEditSecretDeletesVaultValue(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_DELETE_SECRET"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	d, out := newDialog("2\ny\n")
	editSecret(d, trackedSecret{name: name, source: "vault"})
	if _, src, ok := secrets.Get(name); ok || src != "" {
		t.Fatalf("deleted secret still resolves from %q", src)
	}
	if !strings.Contains(out.String(), "deleted "+name) {
		t.Errorf("missing deletion confirmation: %s", out.String())
	}
}

func TestProbeCachedVaultAndEnvOverride(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_CACHED_SECRET"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	if err := secrets.RecordProbe(name, "verified", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	p := fakeKeyedProvider(t, name, "env-value")
	kt := keyTarget{provider: p}
	status, _, age := probeCached(kt)
	if status != "verified" || age < time.Minute {
		t.Fatalf("vault cache = (%q, %v), want verified cached result", status, age)
	}
	t.Setenv(name, "env-value")
	status, _, age = probeCached(kt)
	if status != "verified" || age != 0 {
		t.Fatalf("env override = (%q, %v), want verified live result", status, age)
	}
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
			value, src, found := secrets.Get(name)
			if kind == "secret" && (!found || src != "vault" || value != want.secret) {
				t.Errorf("secret = (%q, %q, %v)", value, src, found)
			}
			if kind == "plain" && found {
				t.Error("plain env value also stored as a secret")
			}
			if !sameAsSaved(name, edit) || strings.Contains(out.String(), "custom-value") {
				t.Errorf("saved edit differs or transcript leaks value: %s", out.String())
			}
		})
	}
}
