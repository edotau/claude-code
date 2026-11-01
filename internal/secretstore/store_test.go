package secretstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The subprocess inherits the parent's t.TempDir-backed vault and master key.
func TestConcurrentStoreHelperStoreOneValue(t *testing.T) {
	name := os.Getenv("CONCURRENCY_HELPER_NAME")
	if name == "" {
		t.Skip("not invoked as the concurrency helper")
	}
	if err := Store(name, "v-"+name); err != nil {
		t.Fatalf("Store(%s): %v", name, err)
	}
}

func TestConcurrentStoreAllLand(t *testing.T) {
	vaulttest.Use(t)
	const processes = 8
	names := make([]string, processes)
	for i := range names {
		names[i] = fmt.Sprintf("CONC_%02d", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			<-start
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConcurrentStoreHelperStoreOneValue$", "-test.v")
			cmd.Env = append(os.Environ(),
				"CONCURRENCY_HELPER_NAME="+n,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs[i] = err
				t.Logf("helper %s output:\n%s", n, out)
			}
		}(i, n)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("helper %s failed: %v", names[i], err)
		}
	}

	if got := Names(); !slices.Equal(got, names) {
		t.Fatalf("Names = %v, want %v", got, names)
	}
	for _, n := range names {
		if value, source, err := Lookup(n); err != nil || value != "v-"+n || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", n, value, source, err, "v-"+n)
		}
	}

	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Errorf("vault metadata missing a row for %s", n)
		}
	}
}

func TestEffectiveVaultOnly(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("VAULT_ONLY", "")
	vaulttest.Secret(t, "VAULT_ONLY", "vault-value")
	v, src, shadowed, ok := Effective("VAULT_ONLY")
	if !ok || v != "vault-value" || src != "vault" || shadowed != "" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (vault-value, vault, \"\", true)", v, src, shadowed, ok)
	}
}

func TestEffectiveEnvOverVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "ENV_OVER_VAULT", "vault-value")
	t.Setenv("ENV_OVER_VAULT", "  env-value \n")
	v, src, shadowed, ok := Effective("ENV_OVER_VAULT")
	if !ok || v != "env-value" || src != "env" || shadowed != "vault" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, vault, true)", v, src, shadowed, ok)
	}
	if gv, gsrc, gok := Get("ENV_OVER_VAULT"); !gok || gv != "vault-value" || gsrc != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (vault-value, vault, true)", gv, gsrc, gok)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if v, src, sh, ok := Effective("ENV_OVER_VAULT"); !ok || v != "env-value" || src != "env" || sh != "vault" {
		t.Fatalf("Effective must detect shadowed vault without a key: (%q, %q, %q, %v)", v, src, sh, ok)
	}
}

func TestEffectiveEnvOnly(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", "env-value")
	if v, src, sh, ok := Effective("ONLY_IN_ENV"); !ok || v != "env-value" || src != "env" || sh != "" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, \"\", true)", v, src, sh, ok)
	}
}

func TestEffectiveMiss(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("NEVER_SET_XYZ", "")
	v, src, shadowed, ok := Effective("NEVER_SET_XYZ")
	if ok || v != "" || src != "" || shadowed != "" {
		t.Fatalf("Effective(miss) = (%q, %q, %q, %v), want all empty, ok=false", v, src, shadowed, ok)
	}
}

func TestEffectiveLockedVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "LOCKED", "vault-value")
	t.Setenv("LOCKED", "")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if v, src, sh, ok := Effective("LOCKED"); ok || v != "" || src != "" || sh != "" {
		t.Fatalf("Effective(locked) = (%q, %q, %q, %v), want empty, false", v, src, sh, ok)
	}
}

func wrongVaultKey(t *testing.T) {
	t.Helper()
	vaulttest.Keyring(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

func TestStoreUsesKeyringMasterKey(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	if err := Store("A_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	if value, source, err := Lookup("A_KEY"); err != nil || value != "v" || source != "vault" {
		t.Fatalf("Lookup = (%q, %q, %v), want (v, vault, nil)", value, source, err)
	}
	if _, source, err := vault.MasterKey(); err != nil || source != "keyring" {
		t.Fatalf("MasterKey source = %q, %v, want keyring, nil", source, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "env.d")); !os.IsNotExist(err) {
		t.Fatalf("Store must not create env.d: %v", err)
	}
}

func TestStoreWithoutMasterKeyFails(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	kr.Down = true
	if err := Store("B_KEY", "v"); !errors.Is(err, vault.ErrLocked) {
		t.Fatalf("Store = %v, want vault.ErrLocked", err)
	}
	if got := Names(); len(got) != 0 {
		t.Fatalf("failed Store persisted names: %v", got)
	}
}

func TestStoreRejectsMultiline(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\r\nb"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Secret(t, "ONE_LINE", "unchanged")
			before := readRecord(t, "ONE_LINE")
			if err := Store("ONE_LINE", value); err == nil || err.Error() != "ONE_LINE: a secret must be one line" {
				t.Fatalf("Store multiline = %v", err)
			}
			after := readRecord(t, "ONE_LINE")
			if !bytes.Equal(before.Sealed, after.Sealed) || before.Kid != after.Kid || !before.SetAt.Equal(after.SetAt) {
				t.Fatal("rejected Store changed the existing record")
			}
		})
	}
}

func TestDeleteRemovesVaultRecordWithoutKey(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "C_KEY", "v")
	vaulttest.Secret(t, "KEPT", "untouched")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("C_KEY", "env-value")
	if err := Delete("C_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"KEPT"}) {
		t.Fatalf("Names after Delete = %v, want [KEPT]", got)
	}
	if value, source, err := Lookup("C_KEY"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup after Delete = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
}

// Delete on a name stored nowhere must be a no-op, not an error.
func TestDeleteMissingIsNoop(t *testing.T) {
	vaulttest.Use(t)
	if err := Delete("NEVER_SET"); err != nil {
		t.Fatalf("Delete on an unset name must not error: %v", err)
	}
}

// Redact must show only the length for a short value, and never the middle of a long one.
func TestRedact(t *testing.T) {
	if got := Redact("short"); got != "5 chars" {
		t.Errorf("Redact(short) = %q, want %q", got, "5 chars")
	}
	if got := Redact("elevenchars"); got != "11 chars" {
		t.Errorf("Redact(11 chars) = %q, want length only", got)
	}
	// A pasted key shows just enough to tell it from a stray clipboard line, never the rest.
	long := "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"
	got := Redact(long)
	if want := "sk-an*** (" + strconv.Itoa(len(long)) + " chars)"; got != want {
		t.Errorf("Redact(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, long[5:]) || strings.Contains(got, long[len(long)-4:]) {
		t.Errorf("Redact(long) leaked the value: %q", got)
	}
	if got := Redact("kJaxH1234567890ab"); got != "kJaxH*** (17 chars)" {
		t.Errorf("Redact(id) = %q", got)
	}
}

func TestStoreGetRoundTripAndPerms(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("GATEWAY_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	fi, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatalf("stat %s: %v", vault.Path(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
	val, source, ok := Get("GATEWAY_API_KEY")
	if !ok || val != "sk-test-abc123" || source != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (sk-test-abc123, vault, true)", val, source, ok)
	}
}

func TestStoreReplacesAndKeepsOthers(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("A_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	if err := Store("B_KEY", "untouched"); err != nil {
		t.Fatal(err)
	}
	if err := Store("A_KEY", "second"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"A_KEY": "second", "B_KEY": "untouched"} {
		if got, source, err := Lookup(name); err != nil || got != want || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", name, got, source, err, want)
		}
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "B_KEY"}) {
		t.Fatalf("Names = %v, want [A_KEY B_KEY]", got)
	}
}

func TestGetFallsThroughToEnvOnlyWhenVaultEmpty(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", " from-process-env \n")
	val, source, ok := Get("ONLY_IN_ENV")
	if !ok || val != "from-process-env" || source != "env" {
		t.Fatalf("Get = (%q, %q, %v), want (from-process-env, env, true)", val, source, ok)
	}
}

// Get on a name set nowhere must miss cleanly, never an error or a panic.
func TestGetMiss(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("NEVER_SET", " \n")
	if v, src, ok := Get("NEVER_SET"); ok || v != "" || src != "" {
		t.Fatal("Get on an unset name must report ok=false")
	}
}

func TestLookupVaultBeforeEnvAndPreservesError(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "LOOKUP", "vault-value")
	vaulttest.Secret(t, "LOOKUP_NO_ENV", "vault-value")
	t.Setenv("LOOKUP", "env-value")
	if value, source, err := Lookup("LOOKUP"); err != nil || value != "vault-value" || source != "vault" {
		t.Fatalf("Lookup = (%q, %q, %v), want (vault-value, vault, nil)", value, source, err)
	}

	// A vault read error with a non-empty env var falls back to it rather than failing outright.
	wrongVaultKey(t)
	if value, source, err := Lookup("LOOKUP"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup with wrong key, env set = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
	if value, source, ok := Get("LOOKUP"); !ok || value != "env-value" || source != "env" {
		t.Fatalf("Get with wrong key, env set = (%q, %q, %v), want (env-value, env, true)", value, source, ok)
	}

	// Same locked vault, no env var: the read error surfaces.
	if value, source, err := Lookup("LOOKUP_NO_ENV"); !errors.Is(err, vault.ErrWrongKey) || value != "" || source != "" {
		t.Fatalf("Lookup with wrong key, no env = (%q, %q, %v), want empty, empty, ErrWrongKey", value, source, err)
	}
	if value, source, ok := Get("LOOKUP_NO_ENV"); ok || value != "" || source != "" {
		t.Fatalf("Get with wrong key, no env = (%q, %q, %v), want empty, empty, false", value, source, ok)
	}

	t.Setenv("ENV_FALLBACK", " env-value ")
	if value, source, err := Lookup("ENV_FALLBACK"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup absent record = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
}

func TestLookupMissingAndEmptyValue(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("LOOKUP_EMPTY", "")
	if value, source, err := Lookup("LOOKUP_EMPTY"); err != nil || value != "" || source != "" {
		t.Fatalf("Lookup missing = (%q, %q, %v), want empty, empty, nil", value, source, err)
	}
	if err := Store("LOOKUP_EMPTY", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOKUP_EMPTY", "env-value")
	if value, source, err := Lookup("LOOKUP_EMPTY"); err != nil || value != "" || source != "vault" {
		t.Fatalf("Lookup empty record = (%q, %q, %v), want empty, vault, nil", value, source, err)
	}
	if value, source, ok := Get("LOOKUP_EMPTY"); ok || value != "" || source != "vault" {
		t.Fatalf("Get empty record = (%q, %q, %v), want empty, vault, false", value, source, ok)
	}
}

func TestNamesUnreadableVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(vault.SchemaVersion+1))
	if got := Names(); got != nil {
		t.Fatalf("Names unreadable vault = %v, want nil", got)
	}
}

// Names remain listable without decryption, including custom secrets unknown to the provider registry.
func TestStoreAndDeleteMaintainNamesIndex(t *testing.T) {
	vaulttest.Use(t)
	for _, n := range []string{"Z_KEY", "A_KEY", "Z_KEY"} {
		if err := Store(n, "secret-value"); err != nil {
			t.Fatal(err)
		}
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names = %q, want [A_KEY Z_KEY]", got)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names without key = %q, want [A_KEY Z_KEY]", got)
	}
	if err := Delete("Z_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY"}) {
		t.Fatalf("Names after Delete = %q, want [A_KEY]", got)
	}
}

func TestPreviewShowsPrefixLengthAndDoubledPaste(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("a", 95)
	got := Preview(key)
	if !strings.HasPrefix(got, "sk-ant-api03-*") || !strings.HasSuffix(got, "(108 chars)") || strings.Contains(got, "aaa") {
		t.Errorf("Preview(anthropic) = %q", got)
	}
	if w := len([]rune("  pasted  " + Preview(key+key))); w > 80 {
		t.Errorf("doubled preview is %d columns", w)
	}
	if !strings.Contains(Preview(key+key), "pasted twice?") {
		t.Errorf("doubled paste not flagged: %q", Preview(key+key))
	}
	if got := Preview("abcdef"); got != "ab**** (6 chars)" {
		t.Errorf("short value reveals too much: %q", got)
	}
	if got := Preview("AIzaSyD" + strings.Repeat("x", 32)); !strings.HasPrefix(got, "AIza***") {
		t.Errorf("Preview(google) = %q", got)
	}
}

func TestPreviewPrefixStopsAtRandomSegment(t *testing.T) {
	cases := map[string]string{
		"tvly-dev-3vjVEqAbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdef": "tvly-dev-",
		"sk-ant-oat01-" + strings.Repeat("Z", 95):                   "sk-ant-oat01-",
		"tskey-api-kAbC123-" + strings.Repeat("q", 42):              "tskey-api-",
		"BBDC-" + strings.Repeat("A", 44):                           "BBDC",
	}
	for in, want := range cases {
		got := Preview(in)
		if !strings.HasPrefix(got, want+"*") {
			t.Errorf("Preview(%.20s…) = %q, want visible prefix %q then stars", in, got, want)
		}
	}
}

// TestLookupVaultErrorFallsBackToEnvOnlyWhenSet: a vault read error (locked key, not "missing")
// returns a non-empty env var with source "env" and no error; with the env var unset, the same
// locked vault surfaces the read error instead of silently reporting a miss.
func TestLookupVaultErrorFallsBackToEnvOnlyWhenSet(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "LOCKED_FALLBACK", "vault-value")
	kr.Down = true

	t.Setenv("LOCKED_FALLBACK", "env-value")
	if value, source, err := Lookup("LOCKED_FALLBACK"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup(env set) = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}

	t.Setenv("LOCKED_FALLBACK", "")
	if value, source, err := Lookup("LOCKED_FALLBACK"); err == nil || value != "" || source != "" {
		t.Fatalf("Lookup(env unset) = (%q, %q, %v), want empty, empty, a read error", value, source, err)
	}
}

// Paste debris at either end is trimmed on every write path; a newline inside the value is still refused.
func TestStoreTrimsPasteDebris(t *testing.T) {
	vaulttest.Use(t)
	for _, raw := range []string{"tok-123\n", "tok-123\r\n", "  tok-123 \t\n\n", "\ntok-123"} {
		if err := Store("PROVIDER_X_REFRESH_TOKEN", raw); err != nil {
			t.Fatalf("Store(%q) = %v, want the debris trimmed", raw, err)
		}
		got, _, err := Lookup("PROVIDER_X_REFRESH_TOKEN")
		if err != nil || got != "tok-123" {
			t.Fatalf("Store(%q) then Lookup = %q, %v; want %q", raw, got, err, "tok-123")
		}
	}
	if err := Store("PROVIDER_X_REFRESH_TOKEN", "tok\n123"); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("Store with an inner newline = %v, want the one-line refusal", err)
	}
}
