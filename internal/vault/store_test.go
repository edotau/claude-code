package vault_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func currentVaultSchema(t *testing.T) int {
	t.Helper()
	for _, f := range schemas.All() {
		if f.Name == "vault" {
			return f.Version
		}
	}
	t.Fatal("vault schema is not registered")
	return 0
}

// The subprocess inherits the parent's t.TempDir-backed vault and master key.
func TestConcurrentStoreHelperStoreOneValue(t *testing.T) {
	name := os.Getenv("CONCURRENCY_HELPER_NAME")
	if name == "" {
		t.Skip("not invoked as the concurrency helper")
	}
	if err := vault.Store(name, "v-"+name); err != nil {
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
	runStoreHelpers(t, names)
	checkAllLanded(t, names)
}

// runStoreHelpers runs one TestConcurrentStoreHelperStoreOneValue process per name at once and fails on any error.
func runStoreHelpers(t *testing.T, names []string) {
	t.Helper()
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
}

// checkAllLanded asserts every name is listed, readable as v-<name> from the vault, and has a metadata row.
func checkAllLanded(t *testing.T, names []string) {
	t.Helper()
	if got := vault.Names(); !slices.Equal(got, names) {
		t.Fatalf("Names = %v, want %v", got, names)
	}
	for _, n := range names {
		if value, source, err := vault.Lookup(n); err != nil || value != "v-"+n || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", n, value, source, err, "v-"+n)
		}
	}

	m, err := vault.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := m[n]; !ok {
			t.Errorf("vault metadata missing a row for %s", n)
		}
	}
}

func TestEffectiveEnvOverVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "ENV_OVER_VAULT", "vault-value")
	t.Setenv("ENV_OVER_VAULT", "  env-value \n")
	v, src, shadowed, ok := vault.Effective("ENV_OVER_VAULT")
	if !ok || v != "env-value" || src != "env" || shadowed != "vault" {
		t.Fatalf("Effective = (%q, %q, %q, %v), want (env-value, env, vault, true)", v, src, shadowed, ok)
	}
	checkGet(t, "ENV_OVER_VAULT", "vault-value", "vault", true, "Get", "(vault-value, vault, true)")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if v, src, sh, ok := vault.Effective("ENV_OVER_VAULT"); !ok || v != "env-value" || src != "env" || sh != "vault" {
		t.Fatalf("Effective must detect shadowed vault without a key: (%q, %q, %q, %v)", v, src, sh, ok)
	}
}

// checkLookup fails with "<what> = (value, source, err), want <want>" unless Lookup(name) returns exactly those.
func checkLookup(t *testing.T, name, value, source string, wantErr error, what, want string) {
	t.Helper()
	if v, src, err := vault.Lookup(name); !errors.Is(err, wantErr) || v != value || src != source {
		t.Fatalf("%s = (%q, %q, %v), want %s", what, v, src, err, want)
	}
}

// checkGet fails with "<what> = (value, source, ok), want <want>" unless Get(name) returns exactly those.
func checkGet(t *testing.T, name, value, source string, ok bool, what, want string) {
	t.Helper()
	if v, src, gok := vault.Get(name); gok != ok || v != value || src != source {
		t.Fatalf("%s = (%q, %q, %v), want %s", what, v, src, gok, want)
	}
}

// Effective reads env before vault and yields nothing when neither holds a usable value.
func TestEffective(t *testing.T) {
	for _, tc := range []struct {
		name, env, stored, key string
		want                   [3]string
		ok                     bool
	}{
		{name: "vault only", stored: "vault-value", want: [3]string{"vault-value", "vault", ""}, ok: true},
		{name: "env only", env: "env-value", want: [3]string{"env-value", "env", ""}, ok: true},
		{name: "miss"},
		{name: "locked vault", stored: "vault-value", key: "invalid-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			if tc.stored != "" {
				vaulttest.Secret(t, "EFFECTIVE_TEST", tc.stored)
			}
			t.Setenv("EFFECTIVE_TEST", tc.env)
			if tc.key != "" {
				t.Setenv("HARNESS_VAULT_KEY", tc.key)
			}
			v, src, sh, ok := vault.Effective("EFFECTIVE_TEST")
			if got := [3]string{v, src, sh}; got != tc.want || ok != tc.ok {
				t.Fatalf("Effective = (%q, %q, %q, %v), want %q, %v", v, src, sh, ok, tc.want, tc.ok)
			}
		})
	}
}

func wrongVaultKey(t *testing.T) {
	t.Helper()
	vaulttest.Keyring(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

func TestStoreUsesKeyringMasterKey(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	if err := vault.Store("A_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	if value, source, err := vault.Lookup("A_KEY"); err != nil || value != "v" || source != "vault" {
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
	if err := vault.Store("B_KEY", "v"); !errors.Is(err, vault.ErrLocked) {
		t.Fatalf("Store = %v, want vault.ErrLocked", err)
	}
	if got := vault.Names(); len(got) != 0 {
		t.Fatalf("failed vault.Store persisted names: %v", got)
	}
}

func TestStoreRejectsMultiline(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\r\nb"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Secret(t, "ONE_LINE", "unchanged")
			before := readRecord(t, "ONE_LINE")
			if err := vault.Store("ONE_LINE", value); err == nil || err.Error() != "ONE_LINE: a secret must be one line" {
				t.Fatalf("Store multiline = %v", err)
			}
			after := readRecord(t, "ONE_LINE")
			if !bytes.Equal(before.Sealed, after.Sealed) || before.Kid != after.Kid || !before.SetAt.Equal(after.SetAt) {
				t.Fatal("rejected vault.Store changed the existing record")
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
	if err := vault.Delete("C_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := vault.Names(); !slices.Equal(got, []string{"KEPT"}) {
		t.Fatalf("Names after vault.Delete = %v, want [KEPT]", got)
	}
	if value, source, err := vault.Lookup("C_KEY"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup after vault.Delete = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
}

// Delete called directly (UI env dialog, PAT rollback) must leave a secret_delete audit event.
func TestDeleteAuditsSecretDelete(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "AUDITED_KEY", "v")
	if err := vault.Delete("AUDITED_KEY"); err != nil {
		t.Fatal(err)
	}
	events, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete && e.Resource == "AUDITED_KEY" {
			return
		}
	}
	t.Fatalf("no secret_delete event for AUDITED_KEY in %d events", len(events))
}

// Delete on a name stored nowhere must be a no-op, not an error, and must not audit a deletion that never happened.
func TestDeleteMissingIsNoop(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Delete("NEVER_SET"); err != nil {
		t.Fatalf("Delete on an unset name must not error: %v", err)
	}
	events, err := audit.Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == audit.SecretDelete && e.Resource == "NEVER_SET" {
			t.Fatalf("Delete of an unset name audited a secret_delete: %+v", e)
		}
	}
}

// Redact must show only the length for a short value, and never the middle of a long one.
func TestRedact(t *testing.T) {
	if got := vault.Redact("short"); got != "5 chars" {
		t.Errorf("Redact(short) = %q, want %q", got, "5 chars")
	}
	if got := vault.Redact("elevenchars"); got != "11 chars" {
		t.Errorf("Redact(11 chars) = %q, want length only", got)
	}
	// A pasted key shows just enough to tell it from a stray clipboard line, never the rest.
	long := "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"
	got := vault.Redact(long)
	if want := "sk-an*** (" + strconv.Itoa(len(long)) + " chars)"; got != want {
		t.Errorf("Redact(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, long[5:]) || strings.Contains(got, long[len(long)-4:]) {
		t.Errorf("Redact(long) leaked the value: %q", got)
	}
	if got := vault.Redact("kJaxH1234567890ab"); got != "kJaxH*** (17 chars)" {
		t.Errorf("Redact(id) = %q", got)
	}
}

func TestStoreGetRoundTripAndPerms(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Store("GATEWAY_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	fi, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatalf("stat %s: %v", vault.Path(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
	val, source, ok := vault.Get("GATEWAY_API_KEY")
	if !ok || val != "sk-test-abc123" || source != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (sk-test-abc123, vault, true)", val, source, ok)
	}
}

func TestStoreReplacesAndKeepsOthers(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Store("A_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	if err := vault.Store("B_KEY", "untouched"); err != nil {
		t.Fatal(err)
	}
	if err := vault.Store("A_KEY", "second"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"A_KEY": "second", "B_KEY": "untouched"} {
		if got, source, err := vault.Lookup(name); err != nil || got != want || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", name, got, source, err, want)
		}
	}
	if got := vault.Names(); !slices.Equal(got, []string{"A_KEY", "B_KEY"}) {
		t.Fatalf("Names = %v, want [A_KEY B_KEY]", got)
	}
}

func TestGetFallsThroughToEnvOnlyWhenVaultEmpty(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", " from-process-env \n")
	val, source, ok := vault.Get("ONLY_IN_ENV")
	if !ok || val != "from-process-env" || source != "env" {
		t.Fatalf("Get = (%q, %q, %v), want (from-process-env, env, true)", val, source, ok)
	}
}

// Get on a name set nowhere must miss cleanly, never an error or a panic.
func TestGetMiss(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("NEVER_SET", " \n")
	if v, src, ok := vault.Get("NEVER_SET"); ok || v != "" || src != "" {
		t.Fatal("Get on an unset name must report ok=false")
	}
}

func TestLookupVaultBeforeEnvAndPreservesError(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "LOOKUP", "vault-value")
	vaulttest.Secret(t, "LOOKUP_NO_ENV", "vault-value")
	t.Setenv("LOOKUP", "env-value")
	checkLookup(t, "LOOKUP", "vault-value", "vault", nil, "Lookup", "(vault-value, vault, nil)")

	// A vault read error with a non-empty env var falls back to it rather than failing outright.
	wrongVaultKey(t)
	checkLookup(t, "LOOKUP", "env-value", "env", nil, "Lookup with wrong key, env set", "(env-value, env, nil)")
	checkGet(t, "LOOKUP", "env-value", "env", true, "Get with wrong key, env set", "(env-value, env, true)")

	// Same locked vault, no env var: the read error surfaces.
	checkLookup(t, "LOOKUP_NO_ENV", "", "", vault.ErrWrongKey,
		"Lookup with wrong key, no env", "empty, empty, ErrWrongKey")
	checkGet(t, "LOOKUP_NO_ENV", "", "", false, "Get with wrong key, no env", "empty, empty, false")

	t.Setenv("ENV_FALLBACK", " env-value ")
	checkLookup(t, "ENV_FALLBACK", "env-value", "env", nil, "Lookup absent record", "(env-value, env, nil)")
}

func TestLookupMissingAndEmptyValue(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("LOOKUP_EMPTY", "")
	checkLookup(t, "LOOKUP_EMPTY", "", "", nil, "Lookup missing", "empty, empty, nil")
	if err := vault.Store("LOOKUP_EMPTY", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOKUP_EMPTY", "env-value")
	checkLookup(t, "LOOKUP_EMPTY", "", "vault", nil, "Lookup empty record", "empty, vault, nil")
	checkGet(t, "LOOKUP_EMPTY", "", "vault", false, "Get empty record", "empty, vault, false")
}

func TestNamesUnreadableVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(currentVaultSchema(t)+1))
	if got := vault.Names(); got != nil {
		t.Fatalf("Names unreadable vault = %v, want nil", got)
	}
}

// Names remain listable without decryption, including custom secrets unknown to the provider registry.
func TestStoreAndDeleteMaintainNamesIndex(t *testing.T) {
	vaulttest.Use(t)
	for _, n := range []string{"Z_KEY", "A_KEY", "Z_KEY"} {
		if err := vault.Store(n, "secret-value"); err != nil {
			t.Fatal(err)
		}
	}
	if got := vault.Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names = %q, want [A_KEY Z_KEY]", got)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	if got := vault.Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names without key = %q, want [A_KEY Z_KEY]", got)
	}
	if err := vault.Delete("Z_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := vault.Names(); !slices.Equal(got, []string{"A_KEY"}) {
		t.Fatalf("Names after vault.Delete = %q, want [A_KEY]", got)
	}
}

func TestPreviewShowsPrefixLengthAndDoubledPaste(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("a", 95)
	got := vault.Preview(key)
	if !strings.HasPrefix(got, "sk-ant-api03-*") || !strings.HasSuffix(got, "(108 chars)") || strings.Contains(got, "aaa") {
		t.Errorf("Preview(anthropic) = %q", got)
	}
	if w := len([]rune("  pasted  " + vault.Preview(key+key))); w > 80 {
		t.Errorf("doubled preview is %d columns", w)
	}
	if !strings.Contains(vault.Preview(key+key), "pasted twice?") {
		t.Errorf("doubled paste not flagged: %q", vault.Preview(key+key))
	}
	if got := vault.Preview("abcdef"); got != "ab**** (6 chars)" {
		t.Errorf("short value reveals too much: %q", got)
	}
	if got := vault.Preview("AIzaSyD" + strings.Repeat("x", 32)); !strings.HasPrefix(got, "AIza***") {
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
		got := vault.Preview(in)
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
	if value, source, err := vault.Lookup("LOCKED_FALLBACK"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup(env set) = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}

	t.Setenv("LOCKED_FALLBACK", "")
	if value, source, err := vault.Lookup("LOCKED_FALLBACK"); err == nil || value != "" || source != "" {
		t.Fatalf("Lookup(env unset) = (%q, %q, %v), want empty, empty, a read error", value, source, err)
	}
}

// Paste debris at either end is trimmed on every write path; a newline inside the value is still refused.
func TestStoreTrimsPasteDebris(t *testing.T) {
	vaulttest.Use(t)
	for _, raw := range []string{"tok-123\n", "tok-123\r\n", "  tok-123 \t\n\n", "\ntok-123"} {
		if err := vault.Store("PROVIDER_X_REFRESH_TOKEN", raw); err != nil {
			t.Fatalf("Store(%q) = %v, want the debris trimmed", raw, err)
		}
		got, _, err := vault.Lookup("PROVIDER_X_REFRESH_TOKEN")
		if err != nil || got != "tok-123" {
			t.Fatalf("Store(%q) then vault.Lookup = %q, %v; want %q", raw, got, err, "tok-123")
		}
	}
	if err := vault.Store("PROVIDER_X_REFRESH_TOKEN", "tok\n123"); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("Store with an inner newline = %v, want the one-line refusal", err)
	}
}

func readRecord(t *testing.T, name string) vault.Record {
	t.Helper()
	var record vault.Record
	if err := vault.View(func(tx *vault.Tx) error {
		var found bool
		var err error
		record, found, err = tx.Record(name)
		if err == nil && !found {
			t.Fatalf("record %s missing", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLoadMetaKeylessAndCurrent(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Store("STORED", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, "STORED")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	for range 2 {
		meta, err := vault.LoadMeta()
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]vault.SecretMeta{"STORED": {
			Prefix: r.Prefix, Len: r.Len, SetAt: r.SetAt,
			VerifiedAt: r.VerifiedAt, VerifyStatus: r.VerifyStatus,
		}}
		if !reflect.DeepEqual(meta, want) {
			t.Fatalf("LoadMeta = %+v, want %+v", meta, want)
		}
	}
	if err := vault.Delete("STORED"); err != nil {
		t.Fatal(err)
	}
	if meta, err := vault.LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta after vault.Delete = %+v, %v", meta, err)
	}
}

func TestLoadMetaMissingVaultDoesNotCreateIt(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if meta, err := vault.LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta = %+v, %v, want empty, nil", meta, err)
	}
	if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
		t.Fatalf("LoadMeta created a vault: %v", err)
	}
}

func TestRecordProbeKeylessPreservesRecord(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "PROBE", "sk-test-abcdefghijklmnopqrstuvwxyz")
	before := readRecord(t, "PROBE")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := vault.RecordProbe("PROBE", "verified", at, before.SetAt); err != nil {
		t.Fatal(err)
	}
	want := before
	want.VerifyStatus, want.VerifiedAt = "verified", at
	if got := readRecord(t, "PROBE"); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordProbe changed fields beyond the probe: got %+v, want %+v", got, want)
	}
	if meta, err := vault.LoadMeta(); err != nil || meta["PROBE"].VerifyStatus != "verified" || !meta["PROBE"].VerifiedAt.Equal(at) {
		t.Fatalf("LoadMeta probe = %+v, %v", meta, err)
	}
	if err := vault.RecordProbe("MISSING", "rejected", at, at); err != nil {
		t.Fatal(err)
	}
	if got := vault.Names(); len(got) != 1 || got[0] != "PROBE" {
		t.Fatalf("RecordProbe created an absent record: %v", got)
	}
}

func TestStoreNeverLeaksRawBytesIntoMetadataOrVault(t *testing.T) {
	vaulttest.Use(t)
	values := []string{"aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStT12", "zZyYxXwWvVuUtTsSrRqQpPoOnNmMlLkKjJiIhHgG98"}
	for i, name := range []string{"FIRST", "SECOND"} {
		if err := vault.Store(name, values[i]); err != nil {
			t.Fatal(err)
		}
	}
	checkNoPlaintext(t, values)
	fi, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode = %04o, want 0600", fi.Mode().Perm())
	}
}

// checkNoPlaintext fails if any value appears in the marshaled metadata or the raw vault file.
func checkNoPlaintext(t *testing.T, values []string) {
	t.Helper()
	meta, err := vault.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	rawMeta, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	rawVault, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if bytes.Contains(rawMeta, []byte(value)) || bytes.Contains(rawVault, []byte(value)) {
			t.Fatal("metadata or vault contains a plaintext secret")
		}
	}
}

func TestStoreClearsVerifyStatusAndDeleteRemovesRow(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.Store("VERIFY_ME", "v1"); err != nil {
		t.Fatal(err)
	}
	setAt := readRecord(t, "VERIFY_ME").SetAt
	at := time.Now()
	if err := vault.RecordProbe("VERIFY_ME", "verified", at, setAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := metaRow(t, "VERIFY_ME"); got.VerifyStatus != "verified" || !got.VerifiedAt.Equal(at) {
		t.Fatalf("probe metadata = %+v", got)
	}
	if err := vault.Store("VERIFY_ME", "v2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := metaRow(t, "VERIFY_ME"); got.VerifyStatus != "" || !got.VerifiedAt.IsZero() {
		t.Fatalf("Store must clear VerifyStatus/VerifiedAt, got %+v", got)
	}
	if err := vault.Delete("VERIFY_ME"); err != nil {
		t.Fatal(err)
	}
	if _, ok := metaRow(t, "VERIFY_ME"); ok {
		t.Fatal("Delete must remove the row entirely")
	}
}

// metaRow loads the vault metadata and returns name's row and whether it exists.
func metaRow(t *testing.T, name string) (vault.SecretMeta, bool) {
	t.Helper()
	m, err := vault.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	row, ok := m[name]
	return row, ok
}

func TestMetaPrefixNeverPersistsWholeValue(t *testing.T) {
	vaulttest.Use(t)
	for _, v := range []string{"abc-def-", "ab-cd-ef-gh-", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"} {
		if err := vault.Store("SHORT_SEG", v); err != nil {
			t.Fatal(err)
		}
		m, err := vault.LoadMeta()
		if err != nil {
			t.Fatal(err)
		}
		if p := m["SHORT_SEG"].Prefix; len(p) > len(v)/3 {
			t.Fatalf("prefix %q persisted for %q: over a third of the value", p, v)
		}
	}
	if p := vault.MetaPrefix("sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); p != "sk-ant-oat01-" {
		t.Fatalf("MetaPrefix dropped a genuine vendor prefix: %q", p)
	}
}

func TestMetaPrefixNoSchemeShowsFirstFive(t *testing.T) {
	for _, tc := range []struct{ v, want string }{
		{"abcdefghijklmnopqrstuvwxyz", "abcde"}, // plain long value: 5 chars
		{"abcdefghijkl", "abcd"},                // 12 chars: a third is 4, under the 5 cap
		{"abcdefghijk", ""},                     // under redactMin: nothing kept
		{"short", ""},
	} {
		if got := vault.MetaPrefix(tc.v); got != tc.want {
			t.Errorf("MetaPrefix(%q) = %q, want %q", tc.v, got, tc.want)
		}
		if got := vault.MetaPrefix(tc.v); len(got) > len(tc.v)/3 {
			t.Errorf("MetaPrefix(%q) = %q is over a third of the value", tc.v, got)
		}
	}
}

func TestCachedProbe(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := time.Hour

	cases := []struct {
		name       string
		m          vault.SecretMeta
		wantStatus string
		wantAge    time.Duration
		wantOK     bool
	}{
		{"verified within ttl", vault.SecretMeta{VerifyStatus: "verified", VerifiedAt: now.Add(-10 * time.Minute)}, "verified", 10 * time.Minute, true},
		{"rejected within ttl", vault.SecretMeta{VerifyStatus: "rejected", VerifiedAt: now.Add(-10 * time.Minute)}, "rejected", 10 * time.Minute, true},
		{"unreachable", vault.SecretMeta{VerifyStatus: "unreachable", VerifiedAt: now.Add(-10 * time.Minute)}, "", 10 * time.Minute, false},
		{"verified older than ttl", vault.SecretMeta{VerifyStatus: "verified", VerifiedAt: now.Add(-2 * time.Hour)}, "", 2 * time.Hour, false},
		{"verified at ttl", vault.SecretMeta{VerifyStatus: "verified", VerifiedAt: now.Add(-ttl)}, "", ttl, false},
		{"zero VerifiedAt", vault.SecretMeta{}, "", 0, false},
	}
	for _, c := range cases {
		status, age, ok := vault.CachedProbe(c.m, ttl, now)
		if status != c.wantStatus || age != c.wantAge || ok != c.wantOK {
			t.Errorf("%s: vault.CachedProbe = (%q, %v, %v), want (%q, %v, %v)",
				c.name, status, age, ok, c.wantStatus, c.wantAge, c.wantOK)
		}
	}
}

func TestLocate(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	for _, tc := range []struct {
		name, env, source, shadowed string
		stored                      bool
	}{
		{"env over vault", "x", "env", "vault", true},
		{"vault only", "", "vault", "", true},
		{"env only", "x", "env", "", false},
		{"none", "", "", "", false},
		{"blank env over vault", " \n", "vault", "", true},
		{"blank env", " \n", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LOCATE_TEST", tc.env)
			meta := map[string]vault.SecretMeta{}
			if tc.stored {
				meta["LOCATE_TEST"] = vault.SecretMeta{}
			}
			if src, sh := vault.Locate("LOCATE_TEST", meta); src != tc.source || sh != tc.shadowed {
				t.Errorf("Locate = (%q, %q), want (%q, %q)", src, sh, tc.source, tc.shadowed)
			}
		})
	}
}
