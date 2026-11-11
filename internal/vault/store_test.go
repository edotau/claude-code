package vault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	if err := Store(name, "v-"+name); err != nil {
		t.Fatalf("Store(%s): %v", name, err)
	}
}

func TestConcurrentStoreAllLand(t *testing.T) {
	useZeroRootVault(t)
	names := make([]string, 8)
	for i := range names {
		names[i] = fmt.Sprintf("CONC_%02d", i)
	}
	runStoreHelpers(t, names)
	if got := Names(); !slices.Equal(got, names) {
		t.Fatalf("Names = %v, want %v", got, names)
	}
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		checkLookup(t, n, "v-"+n, "vault", nil)
		if _, ok := m[n]; !ok {
			t.Errorf("vault metadata missing a row for %s", n)
		}
	}
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
		wg.Go(func() {
			<-start
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConcurrentStoreHelperStoreOneValue$", "-test.v")
			cmd.Env = append(os.Environ(), "CONCURRENCY_HELPER_NAME="+n)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs[i] = err
				t.Logf("helper %s output:\n%s", n, out)
			}
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("helper %s failed: %v", names[i], err)
		}
	}
}

func TestEffectiveEnvOverVault(t *testing.T) {
	useZeroRootVault(t)
	putTestSecret(t, "ENV_OVER_VAULT", "vault-value")
	t.Setenv("ENV_OVER_VAULT", "  env-value \n")
	check := func(what string) {
		t.Helper()
		if v, src, sh, ok := Effective("ENV_OVER_VAULT"); !ok || v != "env-value" || src != "env" || sh != "vault" {
			t.Fatalf("%s: Effective = (%q, %q, %q, %v), want (env-value, env, vault, true)", what, v, src, sh, ok)
		}
	}
	check("with key")
	checkGet(t, "ENV_OVER_VAULT", "vault-value", "vault", true)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	check("shadowed vault must be detected without a key")
}

// checkLookup fails unless Lookup(name) returns exactly (value, source) with an error matching wantErr.
func checkLookup(t *testing.T, name, value, source string, wantErr error) {
	t.Helper()
	if v, src, err := Lookup(name); !errors.Is(err, wantErr) || v != value || src != source {
		t.Fatalf("Lookup(%s) = (%q, %q, %v), want (%q, %q, %v)", name, v, src, err, value, source, wantErr)
	}
}

// checkGet fails unless Get(name) returns exactly (value, source, ok).
func checkGet(t *testing.T, name, value, source string, ok bool) {
	t.Helper()
	if v, src, gok := Get(name); gok != ok || v != value || src != source {
		t.Fatalf("Get(%s) = (%q, %q, %v), want (%q, %q, %v)", name, v, src, gok, value, source, ok)
	}
}

// Effective and EffectiveSecret read env before vault; EffectiveSecret alone surfaces a locked vault's error.
func TestEffective(t *testing.T) {
	for _, tc := range []struct {
		name, env, stored, key string
		want                   [3]string
		ok, secretErr          bool
	}{
		{name: "vault only", stored: "vault-value", want: [3]string{"vault-value", "vault", ""}, ok: true},
		{name: "env only", env: "env-value", want: [3]string{"env-value", "env", ""}, ok: true},
		{name: "env beats vault", env: "  env-value\n", stored: "vault-value", want: [3]string{"env-value", "env", "vault"}, ok: true},
		{name: "whitespace-only env", env: "   ", stored: "vault-value", want: [3]string{"vault-value", "vault", ""}, ok: true},
		{name: "miss"},
		{name: "locked vault", stored: "vault-value", key: "invalid-key", secretErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useZeroRootVault(t)
			if tc.stored != "" {
				putTestSecret(t, "EFFECTIVE_TEST", tc.stored)
			}
			t.Setenv("EFFECTIVE_TEST", tc.env)
			if tc.key != "" {
				t.Setenv("HARNESS_VAULT_KEY", tc.key)
			}
			v, src, sh, ok := Effective("EFFECTIVE_TEST")
			if got := [3]string{v, src, sh}; got != tc.want || ok != tc.ok {
				t.Fatalf("Effective = (%q, %q, %q, %v), want %q, %v", v, src, sh, ok, tc.want, tc.ok)
			}
			v, src, err := EffectiveSecret("EFFECTIVE_TEST")
			if v != tc.want[0] || src != tc.want[1] || (err != nil) != tc.secretErr {
				t.Fatalf("EffectiveSecret = (%q, %q, %v), want (%q, %q, err=%v)", v, src, err, tc.want[0], tc.want[1], tc.secretErr)
			}
		})
	}
}

func TestStoreUsesKeyringMasterKey(t *testing.T) {
	dir, _ := useKeyringVault(t)
	if err := Store("A_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	checkLookup(t, "A_KEY", "v", "vault", nil)
	if _, source, err := MasterKey(); err != nil || source != "keyring" {
		t.Fatalf("MasterKey source = %q, %v, want keyring, nil", source, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "env.d")); !os.IsNotExist(err) {
		t.Fatalf("Store must not create env.d: %v", err)
	}
}

func TestStoreWithoutMasterKeyFails(t *testing.T) {
	_, kr := useKeyringVault(t)
	kr.err = ErrKeyringUnavailable
	if err := Store("B_KEY", "v"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Store = %v, want ErrLocked", err)
	}
	if got := Names(); len(got) != 0 {
		t.Fatalf("failed Store persisted names: %v", got)
	}
}

func TestStoreRejectsMultiline(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\r\nb"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			useZeroRootVault(t)
			putTestSecret(t, "ONE_LINE", "unchanged")
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
	useZeroRootVault(t)
	putTestSecret(t, "C_KEY", "v")
	putTestSecret(t, "KEPT", "untouched")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("C_KEY", "env-value")
	if err := Delete("C_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"KEPT"}) {
		t.Fatalf("Names after Delete = %v, want [KEPT]", got)
	}
	checkLookup(t, "C_KEY", "env-value", "env", nil)
}

// A direct Delete (UI env dialog, PAT rollback) audits secret_delete; deleting an unset name is a no-op that audits nothing.
func TestDeleteAudit(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		stored    bool
	}{
		{"stored secret is audited", "AUDITED_KEY", true},
		{"missing name is a silent no-op", "NEVER_SET", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useZeroRootVault(t)
			if tc.stored {
				putTestSecret(t, tc.key, "v")
			}
			if err := Delete(tc.key); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			events, err := audit.Read(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			audited := slices.ContainsFunc(events, func(e audit.Event) bool {
				return e.Action == audit.SecretDelete && e.Resource == tc.key
			})
			if audited != tc.stored {
				t.Fatalf("secret_delete audited = %v, want %v (%d events)", audited, tc.stored, len(events))
			}
		})
	}
}

// Redact shows only the length for a short value, and never the middle of a long one.
func TestRedact(t *testing.T) {
	long := "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"
	for in, want := range map[string]string{
		"short":             "5 chars",
		"elevenchars":       "11 chars",
		long:                "sk-an*** (" + strconv.Itoa(len(long)) + " chars)",
		"kJaxH1234567890ab": "kJaxH*** (17 chars)",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Redact(long); strings.Contains(got, long[5:]) || strings.Contains(got, long[len(long)-4:]) {
		t.Errorf("Redact(long) leaked the value: %q", got)
	}
}

func TestStoreGetRoundTripAndPerms(t *testing.T) {
	useZeroRootVault(t)
	if err := Store("GATEWAY_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	checkVaultMode(t)
	checkGet(t, "GATEWAY_API_KEY", "sk-test-abc123", "vault", true)
}

func checkVaultMode(t *testing.T) {
	t.Helper()
	fi, err := os.Stat(Path())
	if err != nil {
		t.Fatalf("stat %s: %v", Path(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("vault mode = %04o, want 0600", perm)
	}
}

func TestStoreReplacesAndKeepsOthers(t *testing.T) {
	useZeroRootVault(t)
	for _, kv := range [][2]string{{"A_KEY", "first"}, {"B_KEY", "untouched"}, {"A_KEY", "second"}} {
		if err := Store(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	checkLookup(t, "A_KEY", "second", "vault", nil)
	checkLookup(t, "B_KEY", "untouched", "vault", nil)
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "B_KEY"}) {
		t.Fatalf("Names = %v, want [A_KEY B_KEY]", got)
	}
}

func TestGetEnvAndMiss(t *testing.T) {
	useZeroRootVault(t)
	t.Setenv("ONLY_IN_ENV", " from-process-env \n")
	checkGet(t, "ONLY_IN_ENV", "from-process-env", "env", true)
	t.Setenv("NEVER_SET", " \n")
	checkGet(t, "NEVER_SET", "", "", false)
}

func TestLookupVaultBeforeEnvAndPreservesError(t *testing.T) {
	useZeroRootVault(t)
	putTestSecret(t, "LOOKUP", "vault-value")
	putTestSecret(t, "LOOKUP_NO_ENV", "vault-value")
	t.Setenv("LOOKUP", "env-value")
	checkLookup(t, "LOOKUP", "vault-value", "vault", nil)

	// A vault read error with a non-empty env var falls back to it rather than failing outright.
	useWrongVaultKey(t)
	checkLookup(t, "LOOKUP", "env-value", "env", nil)
	checkGet(t, "LOOKUP", "env-value", "env", true)

	// Same locked vault, no env var: the read error surfaces.
	checkLookup(t, "LOOKUP_NO_ENV", "", "", ErrWrongKey)
	checkGet(t, "LOOKUP_NO_ENV", "", "", false)

	t.Setenv("ENV_FALLBACK", " env-value ")
	checkLookup(t, "ENV_FALLBACK", "env-value", "env", nil)
}

func TestLookupMissingAndEmptyValue(t *testing.T) {
	useZeroRootVault(t)
	t.Setenv("LOOKUP_EMPTY", "")
	checkLookup(t, "LOOKUP_EMPTY", "", "", nil)
	if err := Store("LOOKUP_EMPTY", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOKUP_EMPTY", "env-value")
	checkLookup(t, "LOOKUP_EMPTY", "", "vault", nil)
	checkGet(t, "LOOKUP_EMPTY", "", "vault", false)
}

// A locked-vault read error returns a set env var as source "env", but surfaces as an error when the env var is unset.
func TestLookupVaultErrorFallsBackToEnvOnlyWhenSet(t *testing.T) {
	_, kr := useKeyringVault(t)
	putTestSecret(t, "LOCKED_FALLBACK", "vault-value")
	kr.err = ErrKeyringUnavailable
	t.Setenv("LOCKED_FALLBACK", "env-value")
	checkLookup(t, "LOCKED_FALLBACK", "env-value", "env", nil)
	t.Setenv("LOCKED_FALLBACK", "")
	if value, source, err := Lookup("LOCKED_FALLBACK"); err == nil || value != "" || source != "" {
		t.Fatalf("Lookup(env unset) = (%q, %q, %v), want empty, empty, a read error", value, source, err)
	}
}

func TestNamesUnreadableVault(t *testing.T) {
	useZeroRootVault(t)
	putVault(t, Meta, "schema", strconv.Itoa(currentVaultSchema(t)+1))
	if got := Names(); got != nil {
		t.Fatalf("Names unreadable vault = %v, want nil", got)
	}
}

// Names remain listable without decryption, including custom secrets unknown to the provider registry.
func TestStoreAndDeleteMaintainNamesIndex(t *testing.T) {
	useZeroRootVault(t)
	for _, n := range []string{"Z_KEY", "A_KEY", "Z_KEY"} {
		if err := Store(n, "secret-value"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"A_KEY", "Z_KEY"}
	if got := Names(); !slices.Equal(got, want) {
		t.Fatalf("Names = %q, want %q", got, want)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	if got := Names(); !slices.Equal(got, want) {
		t.Fatalf("Names without key = %q, want %q", got, want)
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
	doubled := Preview(key + key)
	if w := len([]rune("  pasted  " + doubled)); w > 80 {
		t.Errorf("doubled preview is %d columns", w)
	}
	if !strings.Contains(doubled, "pasted twice?") {
		t.Errorf("doubled paste not flagged: %q", doubled)
	}
	if got := Preview("abcdef"); got != "ab**** (6 chars)" {
		t.Errorf("short value reveals too much: %q", got)
	}
	if got := Preview("AIzaSyD" + strings.Repeat("x", 32)); !strings.HasPrefix(got, "AIza***") {
		t.Errorf("Preview(google) = %q", got)
	}
}

func TestPreviewPrefixStopsAtRandomSegment(t *testing.T) {
	for in, want := range map[string]string{
		"tvly-dev-3vjVEqAbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdef": "tvly-dev-",
		"sk-ant-oat01-" + strings.Repeat("Z", 95):                   "sk-ant-oat01-",
		"tskey-api-kAbC123-" + strings.Repeat("q", 42):              "tskey-api-",
		"BBDC-" + strings.Repeat("A", 44):                           "BBDC",
	} {
		if got := Preview(in); !strings.HasPrefix(got, want+"*") {
			t.Errorf("Preview(%.20s…) = %q, want visible prefix %q then stars", in, got, want)
		}
	}
}

// Paste debris at either end is trimmed on every write path; a newline inside the value is still refused.
func TestStoreTrimsPasteDebris(t *testing.T) {
	useZeroRootVault(t)
	for _, raw := range []string{"tok-123\n", "tok-123\r\n", "  tok-123 \t\n\n", "\ntok-123"} {
		if err := Store("PROVIDER_X_REFRESH_TOKEN", raw); err != nil {
			t.Fatalf("Store(%q) = %v, want the debris trimmed", raw, err)
		}
		checkLookup(t, "PROVIDER_X_REFRESH_TOKEN", "tok-123", "vault", nil)
	}
	if err := Store("PROVIDER_X_REFRESH_TOKEN", "tok\n123"); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("Store with an inner newline = %v, want the one-line refusal", err)
	}
}

func readRecord(t *testing.T, name string) Record {
	t.Helper()
	var record Record
	if err := View(func(tx *Tx) error {
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
	useZeroRootVault(t)
	if err := Store("STORED", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatal(err)
	}
	r := readRecord(t, "STORED")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	want := map[string]SecretMeta{"STORED": {
		Prefix: r.Prefix, Len: r.Len, SetAt: r.SetAt, VerifiedAt: r.VerifiedAt, VerifyStatus: r.VerifyStatus,
	}}
	for range 2 {
		if meta, err := LoadMeta(); err != nil || !reflect.DeepEqual(meta, want) {
			t.Fatalf("LoadMeta = %+v, %v, want %+v", meta, err, want)
		}
	}
	if err := Delete("STORED"); err != nil {
		t.Fatal(err)
	}
	if meta, err := LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta after Delete = %+v, %v", meta, err)
	}
}

func TestLoadMetaMissingVaultDoesNotCreateIt(t *testing.T) {
	useZeroRootVault(t)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	if meta, err := LoadMeta(); err != nil || len(meta) != 0 {
		t.Fatalf("LoadMeta = %+v, %v, want empty, nil", meta, err)
	}
	if _, err := os.Stat(Path()); !os.IsNotExist(err) {
		t.Fatalf("LoadMeta created a vault: %v", err)
	}
}

func TestRecordProbeKeylessPreservesRecord(t *testing.T) {
	useZeroRootVault(t)
	putTestSecret(t, "PROBE", "sk-test-abcdefghijklmnopqrstuvwxyz")
	before := readRecord(t, "PROBE")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := RecordProbe("PROBE", "verified", at, before.SetAt); err != nil {
		t.Fatal(err)
	}
	want := before
	want.VerifyStatus, want.VerifiedAt = "verified", at
	if got := readRecord(t, "PROBE"); !reflect.DeepEqual(got, want) {
		t.Fatalf("RecordProbe changed fields beyond the probe: got %+v, want %+v", got, want)
	}
	if got, _ := metaRow(t, "PROBE"); got.VerifyStatus != "verified" || !got.VerifiedAt.Equal(at) {
		t.Fatalf("LoadMeta probe = %+v", got)
	}
	if err := RecordProbe("MISSING", "rejected", at, at); err != nil {
		t.Fatal(err)
	}
	if got := Names(); len(got) != 1 || got[0] != "PROBE" {
		t.Fatalf("RecordProbe created an absent record: %v", got)
	}
}

func TestStoreNeverLeaksRawBytesIntoMetadataOrVault(t *testing.T) {
	useZeroRootVault(t)
	values := []string{"aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStT12", "zZyYxXwWvVuUtTsSrRqQpPoOnNmMlLkKjJiIhHgG98"}
	for i, name := range []string{"FIRST", "SECOND"} {
		if err := Store(name, values[i]); err != nil {
			t.Fatal(err)
		}
	}
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	rawMeta, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rawVault, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if bytes.Contains(rawMeta, []byte(value)) || bytes.Contains(rawVault, []byte(value)) {
			t.Fatal("metadata or vault contains a plaintext secret")
		}
	}
	checkVaultMode(t)
}

func TestStoreClearsVerifyStatusAndDeleteRemovesRow(t *testing.T) {
	useZeroRootVault(t)
	if err := Store("VERIFY_ME", "v1"); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := RecordProbe("VERIFY_ME", "verified", at, readRecord(t, "VERIFY_ME").SetAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := metaRow(t, "VERIFY_ME"); got.VerifyStatus != "verified" || !got.VerifiedAt.Equal(at) {
		t.Fatalf("probe metadata = %+v", got)
	}
	if err := Store("VERIFY_ME", "v2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := metaRow(t, "VERIFY_ME"); got.VerifyStatus != "" || !got.VerifiedAt.IsZero() {
		t.Fatalf("Store must clear VerifyStatus/VerifiedAt, got %+v", got)
	}
	if err := Delete("VERIFY_ME"); err != nil {
		t.Fatal(err)
	}
	if _, ok := metaRow(t, "VERIFY_ME"); ok {
		t.Fatal("Delete must remove the row entirely")
	}
}

// metaRow loads the vault metadata and returns name's row and whether it exists.
func metaRow(t *testing.T, name string) (SecretMeta, bool) {
	t.Helper()
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	row, ok := m[name]
	return row, ok
}

func TestMetaPrefixNeverPersistsWholeValue(t *testing.T) {
	useZeroRootVault(t)
	for _, v := range []string{"abc-def-", "ab-cd-ef-gh-", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"} {
		if err := Store("SHORT_SEG", v); err != nil {
			t.Fatal(err)
		}
		if row, _ := metaRow(t, "SHORT_SEG"); len(row.Prefix) > len(v)/3 {
			t.Fatalf("prefix %q persisted for %q: over a third of the value", row.Prefix, v)
		}
	}
	if p := MetaPrefix("sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); p != "sk-ant-oat01-" {
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
		got := MetaPrefix(tc.v)
		if got != tc.want || len(got) > len(tc.v)/3 {
			t.Errorf("MetaPrefix(%q) = %q, want %q (at most a third of the value)", tc.v, got, tc.want)
		}
	}
}

func TestCachedProbe(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := time.Hour
	meta := func(status string, age time.Duration) SecretMeta {
		return SecretMeta{VerifyStatus: status, VerifiedAt: now.Add(-age)}
	}
	for _, c := range []struct {
		name       string
		m          SecretMeta
		wantStatus string
		wantAge    time.Duration
		wantOK     bool
	}{
		{"verified within ttl", meta("verified", 10*time.Minute), "verified", 10 * time.Minute, true},
		{"rejected within ttl", meta("rejected", 10*time.Minute), "rejected", 10 * time.Minute, true},
		{"unreachable", meta("unreachable", 10*time.Minute), "", 10 * time.Minute, false},
		{"verified older than ttl", meta("verified", 2*time.Hour), "", 2 * time.Hour, false},
		{"verified at ttl", meta("verified", ttl), "", ttl, false},
		{"zero VerifiedAt", SecretMeta{}, "", 0, false},
	} {
		status, age, ok := CachedProbe(c.m, ttl, now)
		if status != c.wantStatus || age != c.wantAge || ok != c.wantOK {
			t.Errorf("%s: CachedProbe = (%q, %v, %v), want (%q, %v, %v)",
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
			meta := map[string]SecretMeta{}
			if tc.stored {
				meta["LOCATE_TEST"] = SecretMeta{}
			}
			if src, sh := Locate("LOCATE_TEST", meta); src != tc.source || sh != tc.shadowed {
				t.Errorf("Locate = (%q, %q), want (%q, %q)", src, sh, tc.source, tc.shadowed)
			}
		})
	}
}

// specMasterKey materializes the vault first: the HKDF subkey is salted by vault_id, which the first write persists.
func specMasterKey(t *testing.T) *Key {
	t.Helper()
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	key, source, err := MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || source != "env" {
		t.Fatalf("MasterKey = %v, %q; want non-nil env key", key, source)
	}
	return key
}

func TestSealOpenRoundTrip(t *testing.T) {
	useZeroRootVault(t)
	key := specMasterKey(t)
	for _, value := range []string{"", "representative-token", "line one\nline two\x00end"} {
		t.Run(fmt.Sprintf("bytes_%d", len(value)), func(t *testing.T) {
			sealed := key.Seal("TOKEN_A", value)
			if len(sealed) != len(value)+28 {
				t.Errorf("sealed length = %d, want %d (96-bit nonce + GCM tag)", len(sealed), len(value)+28)
			}
			got, err := key.Open("TOKEN_A", Record{Sealed: sealed, Kid: key.Kid})
			if err != nil || got != value {
				t.Fatalf("Open round trip = %q, %v; want %q, nil", got, err, value)
			}
		})
	}
	putTestSecret(t, "STORED_TOKEN", "stored-round-trip")
	record := readRecord(t, "STORED_TOKEN")
	if got, err := key.Open("STORED_TOKEN", record); err != nil || got != "stored-round-trip" {
		t.Errorf("stored round trip = %q, %v", got, err)
	}
}

func TestSealOpenRejectsAADSwap(t *testing.T) {
	useZeroRootVault(t)
	key := specMasterKey(t)
	record := Record{Sealed: key.Seal("TOKEN_A", "bound-to-A"), Kid: key.Kid}
	if got, err := key.Open("TOKEN_A", record); err != nil || got != "bound-to-A" {
		t.Fatalf("same-name control failed: got=%q err=%v", got, err)
	}
	if got, err := key.Open("TOKEN_B", record); err == nil {
		t.Fatalf("AAD swap accepted record sealed for TOKEN_A as TOKEN_B: got=%q", got)
	}
	got, err := specSecretsAEAD(t).Open(nil, nil, record.Sealed, []byte("claude-code/vault/v1/secrets/TOKEN_A"))
	if err != nil || string(got) != "bound-to-A" {
		t.Fatalf("ciphertext is not bound to the specified AAD prefix and name: got=%q err=%v", got, err)
	}
}

// specSecretsAEAD derives the secrets AEAD from HARNESS_VAULT_KEY and vault_id per the spec, not via package code.
func specSecretsAEAD(t *testing.T) cipher.AEAD {
	t.Helper()
	rawKey, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	var vaultID string
	if err := View(func(tx *Tx) error {
		vaultID, _ = tx.Get(Meta, "vault_id")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := hkdf.Key(sha256.New, rawKey, []byte(vaultID), "claude-code/vault/v1/secrets", 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func TestSealOpenWrongKid(t *testing.T) {
	useZeroRootVault(t)
	key := specMasterKey(t)
	record := Record{Sealed: key.Seal("TOKEN_A", "value"), Kid: "not-" + key.Kid}
	if _, err := key.Open("TOKEN_A", record); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("Open with wrong kid = %v, want ErrWrongKey", err)
	}
}

func TestViewMissingVaultIsEmptyWithoutCreatingFile(t *testing.T) {
	dir := useZeroRootVault(t)
	wantPath := filepath.Join(dir, "state", "vault.db")
	if got := Path(); got != wantPath {
		t.Fatalf("Path = %q, want %q", got, wantPath)
	}
	called := false
	if err := View(func(tx *Tx) error {
		called = true
		checkEmptyTx(t, tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("View did not call the callback on a missing vault")
	}
	if _, err := os.Stat(wantPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("View created a vault or stat failed unexpectedly: %v", err)
	}
}

// checkEmptyTx asserts every bucket read on tx is empty and no record decodes.
func checkEmptyTx(t *testing.T, tx *Tx) {
	t.Helper()
	for _, bucket := range []string{Pins, Env, Overlay, Secrets, Meta} {
		if value, ok := tx.Get(bucket, "absent"); ok || value != "" {
			t.Errorf("missing %s Get = %q, %t", bucket, value, ok)
		}
		if len(tx.All(bucket)) != 0 || len(tx.Names(bucket)) != 0 {
			t.Errorf("missing %s is not empty", bucket)
		}
	}
	if record, ok, err := tx.Record("absent"); ok || err != nil || !reflect.DeepEqual(record, Record{}) {
		t.Errorf("missing Record = %+v, %t, %v", record, ok, err)
	}
	if recs, err := tx.Records(); err != nil || len(recs) != 0 {
		t.Errorf("missing vault has records or errored: %v, %v", recs, err)
	}
}

func TestConcurrentUpdatePreservesEightKeys(t *testing.T) {
	useZeroRootVault(t)
	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := range writers {
		go func() {
			<-start
			results <- Update(func(tx *Tx) error {
				return tx.Put(Pins, fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i))
			})
		}()
	}
	close(start)
	for range writers {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("concurrent Update: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent Update did not finish within writer timeout")
		}
	}
	if err := View(func(tx *Tx) error {
		if all := tx.All(Pins); len(all) != writers {
			t.Errorf("got %d keys, want %d: %v", len(all), writers, all)
		}
		for i := range writers {
			name, want := fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i)
			if got, ok := tx.Get(Pins, name); !ok || got != want {
				t.Errorf("Get(%q) = %q, %t; want %q, true", name, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewWaitsForHeldUpdate(t *testing.T) {
	useZeroRootVault(t)
	putVault(t, Pins, "held", "before")
	held, release := make(chan struct{}), make(chan struct{})
	writeDone, readDone := make(chan error, 1), make(chan error, 1)
	go func() {
		writeDone <- Update(func(tx *Tx) error {
			close(held)
			<-release
			return tx.Put(Pins, "held", "after")
		})
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-held:
	case err := <-writeDone:
		t.Fatalf("Update ended before acquiring transaction: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Update never acquired the write transaction")
	}
	go func() {
		readDone <- View(func(tx *Tx) error {
			if got, ok := tx.Get(Pins, "held"); !ok || got != "after" {
				return fmt.Errorf("reader saw %q, %t; want committed value after, true", got, ok)
			}
			return nil
		})
	}()
	select {
	case err := <-readDone:
		t.Fatalf("View returned while write transaction was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	released = true
	for name, done := range map[string]chan error{"writer": writeDone, "reader": readDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s failed after release: %v", name, err)
			}
		case <-time.After(6 * time.Second):
			t.Errorf("%s did not finish after release", name)
		}
	}
}

func TestViewRejectsNewerSchema(t *testing.T) {
	useZeroRootVault(t)
	putVault(t, Meta, "schema", "999")
	err := View(func(tx *Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "newer claude-code") {
		t.Fatalf("View schema 3 = %v, want error containing newer claude-code", err)
	}
}

// rawKeyAndKid builds a 32-byte master key with every byte b and its sha256(root)[:16] hex kid, independent of the package.
func rawKeyAndKid(b byte) (b64, kid string) {
	raw := bytes.Repeat([]byte{b}, 32)
	sum := sha256.Sum256(raw)
	return base64.StdEncoding.EncodeToString(raw), hex.EncodeToString(sum[:])[:16]
}

// writeKeyCommandScript writes an executable shell script and returns meta/key_command's JSON argv for it.
func writeKeyCommandScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key_command.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return string(argv)
}

// sealNameUnder stores a sealed NAME record under the key whose bytes are all b, stamping meta/kid with that key.
func sealNameUnder(t *testing.T, b byte) (kid string) {
	t.Helper()
	b64, kid := rawKeyAndKid(b)
	t.Setenv("HARNESS_VAULT_KEY", b64)
	if err := UpdateSealed(func(tx *Tx, k *Key) error {
		if k.Kid != kid {
			t.Fatalf("fixture key kid = %s, want %s", k.Kid, kid)
		}
		return tx.PutRecord("NAME", Record{Sealed: k.Seal("NAME", "secret-value"), Kid: k.Kid})
	}); err != nil {
		t.Fatal(err)
	}
	return kid
}

// A mismatched HARNESS_VAULT_KEY is one candidate source: a key_command supplying the right key still wins.
func TestResolveKeyEnvMismatchFallsThroughToWorkingKeyCommand(t *testing.T) {
	useKeyringVault(t)
	goodKid := sealNameUnder(t, 0x11)
	goodB64, _ := rawKeyAndKid(0x11)
	badB64, badKid := rawKeyAndKid(0x22)
	if goodKid == badKid {
		t.Fatal("fixture kids collide")
	}
	t.Setenv("HARNESS_VAULT_KEY", badB64)
	putVault(t, Meta, "key_command", writeKeyCommandScript(t, "printf '%s' '"+goodB64+"'"))

	value, found, err := ReadSecret("NAME")
	if err != nil {
		t.Fatalf("ReadSecret with a mismatched env key but a working key_command: %v", err)
	}
	if !found || value != "secret-value" {
		t.Fatalf("ReadSecret = (%q, %v), want (secret-value, true)", value, found)
	}
	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey: %v", err)
	}
	if source != "command" || key.Kid != goodKid {
		t.Errorf("MasterKey resolved via %q kid %q, want command/%s", source, key.Kid, goodKid)
	}
}

// With no key_command and nothing in the keyring for the recorded kid, a mismatched env key fails wrapping ErrWrongKey.
func TestResolveKeyEnvMismatchNoFallbackErrorsWrongKey(t *testing.T) {
	useKeyringVault(t)
	goodKid := sealNameUnder(t, 0x33)
	badB64, badKid := rawKeyAndKid(0x44)
	if goodKid == badKid {
		t.Fatal("fixture kids collide")
	}
	t.Setenv("HARNESS_VAULT_KEY", badB64)
	_, _, err := MasterKey()
	if err == nil {
		t.Fatal("MasterKey succeeded with a mismatched env key and no fallback")
	}
	if !errors.Is(err, ErrWrongKey) {
		t.Errorf("MasterKey error = %v, want errors.Is ErrWrongKey", err)
	}
}

// A failing key_command's error carries the command's stderr, so a misconfigured command is diagnosable.
func TestKeyCommandFailureIncludesStderr(t *testing.T) {
	useKeyringVault(t)
	const msg = "custom fail message for stderr capture"
	putVault(t, Meta, "key_command", writeKeyCommandScript(t, "echo '"+msg+"' >&2\nexit 7"))
	_, _, err := MasterKey()
	if err == nil {
		t.Fatal("MasterKey succeeded with a failing key_command")
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("MasterKey error = %q, want it to contain the command's stderr %q", err.Error(), msg)
	}
}

func TestRetiredMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "missing"},
		{name: "empty", raw: `[]`, want: []string{}},
		{name: "null", raw: `null`},
		{name: "valid", raw: `["old", "older"]`, want: []string{"old", "older"}},
		{name: "invalid JSON", raw: `[`, wantErr: true},
		{name: "wrong shape", raw: `{}`, wantErr: true},
		{name: "mixed types", raw: `["old", 42]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useZeroRootVault(t)
			if tc.raw != "" {
				putVault(t, Meta, "retired", tc.raw)
			}
			if err := View(func(tx *Tx) error {
				got, err := Retired(tx)
				if (err != nil) != tc.wantErr {
					t.Fatalf("Retired error = %v, want error %t", err, tc.wantErr)
				}
				if tc.wantErr && !strings.Contains(err.Error(), "retired") {
					t.Errorf("Retired error lacks metadata context: %v", err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("Retired = %v, want %v", got, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetiredAfterTwoRotations(t *testing.T) {
	useKeyringVault(t)
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	old, next, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if old != k.Kid {
		t.Fatalf("old = %s, want %s", old, k.Kid)
	}
	second, _, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if second != next {
		t.Fatalf("second old = %s, want %s", second, next)
	}
	if err := View(func(tx *Tx) error {
		got, err := Retired(tx)
		if want := []string{k.Kid, next}; err == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("Retired = %v, want %v", got, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// RotateKey refuses to rotate out from under HARNESS_VAULT_KEY or meta/key_command unless forced.
func TestRotateKeyRefusesWithoutForce(t *testing.T) {
	for _, tc := range []struct {
		name, mention string
		setup         func(t *testing.T, k *Key)
	}{
		{"env set", "HARNESS_VAULT_KEY", func(t *testing.T, k *Key) {
			// A key with a different kid keeps MasterKey resolving via the keyring while the env var is set.
			for b := range byte(255) {
				if b64, kid := rawKeyAndKid(b); kid != k.Kid {
					t.Setenv("HARNESS_VAULT_KEY", b64)
					break
				}
			}
			if _, source, err := MasterKey(); err != nil || source != "keyring" {
				t.Fatalf("fixture setup: MasterKey = source %q err %v, want keyring/nil", source, err)
			}
		}},
		{"key_command set", "key_command", func(t *testing.T, _ *Key) {
			putVault(t, Meta, "key_command", `["/bin/true"]`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useKeyringVault(t)
			k, err := EnsureKey()
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, k)
			if _, _, err := RotateKey(); err == nil || !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("RotateKey without --force = %v, want a refusal naming %s", err, tc.mention)
			}
		})
	}
}

// An unparsable meta/retired refuses the rotation outright instead of overwriting the retired-kid history.
func TestRotateKeyRefusesWhenRetiredUnparsable(t *testing.T) {
	useKeyringVault(t)
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	putVault(t, Meta, "retired", "not valid json")
	if _, _, err = RotateKey(); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("RotateKey with unparsable meta/retired = %v, want an error naming meta/retired", err)
	}
	if err := View(func(tx *Tx) error {
		if raw, _ := tx.Get(Meta, "retired"); raw != "not valid json" {
			t.Errorf("meta/retired = %q, want it left untouched by the refused rotation", raw)
		}
		if kid, _ := tx.Get(Meta, "kid"); kid != k.Kid {
			t.Errorf("meta/kid = %q, want it still %q (rotation must not have committed)", kid, k.Kid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ForceRotate lets rotation proceed past the key_command guard.
func TestRotateKeyForceBypassesKeyCommandGuard(t *testing.T) {
	useKeyringVault(t)
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	putVault(t, Meta, "key_command", `["/bin/true"]`)
	oldKid, newKid, err := RotateKey(ForceRotate())
	if err != nil {
		t.Fatalf("RotateKey(ForceRotate()) = %v, want success", err)
	}
	if oldKid != k.Kid {
		t.Errorf("oldKid = %q, want %q", oldKid, k.Kid)
	}
	if newKid == "" || newKid == oldKid {
		t.Errorf("newKid = %q, want a fresh non-empty kid", newKid)
	}
}
