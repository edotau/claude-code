package configure

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
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
	editSecret(d, trackedSecret{name: name, source: "vault"})

	if _, staged := d.pending[name]; staged {
		t.Error("the staged Store must be dropped once Delete is picked")
	}
	if !d.pendingDeletes[name] {
		t.Error("the delete must be staged in pendingDeletes")
	}
	if !strings.Contains(out.String(), "staged delete of "+name) {
		t.Errorf("output = %q, want a staged-delete confirmation", out.String())
	}
	if got := os.Getenv(name); got != "" {
		t.Errorf("env after dropping the staged Store = %q, want restored to empty", got)
	}
	if v, _, ok := secrets.Get(name); !ok || v != "stored-value" {
		t.Fatalf("vault value changed before commit: (%q, %v), want still stored-value", v, ok)
	}
	events, err := audit.Read(since)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete {
			t.Fatalf("secret_delete audited before the staged delete was committed: %+v", e)
		}
	}

	// Commit the page's staged changes, as secretsLoop's defer would on leaving the page.
	confirmPageKeys(d, map[string]stagedKey{})
	if _, _, ok := secrets.Get(name); ok {
		t.Error("the vault value should be deleted once the staged delete commits")
	}
	events, err = audit.Read(since)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == audit.SecretDelete && e.Resource == name {
			found = true
		}
	}
	if !found {
		t.Errorf("commit did not audit secret_delete for %s: %+v", name, events)
	}
}

func TestProbeCachedVaultAndEnvOverride(t *testing.T) {
	vaulttest.UseKeyring(t)
	const name = "CONFIGURE_CACHED_SECRET"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "stored-value")
	meta, err := secrets.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.RecordProbe(name, "verified", time.Now().Add(-time.Minute), meta[name].SetAt); err != nil {
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
			if err := secrets.Store(tc.name, "sealed-value"); err != nil {
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
			_, _, found := secrets.Get(tc.name)
			if found == tc.wantDeleted {
				t.Errorf("sealed secret found=%v, want found=%v", found, !tc.wantDeleted)
			}
			if _, ok := providers.CustomEnv()[tc.name]; ok {
				t.Error("vault env entry should have been removed either way")
			}
		})
	}
}
