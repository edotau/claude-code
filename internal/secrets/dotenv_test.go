package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestParseDotenvStrictFormat(t *testing.T) {
	in := "# comment\nENVIRONMENT=dev\n\nURL=https://x?a=b\nexport FOO=bar\nQUOTED=\"kept as is\"\nEMPTY=\n9BAD=x\nno equals\n"
	pairs, problems := ParseDotenv(strings.NewReader(in))
	want := []Pair{{"ENVIRONMENT", "dev"}, {"URL", "https://x?a=b"}, {"FOO", "bar"}, {"QUOTED", "kept as is"}}
	if !slices.Equal(pairs, want) {
		t.Errorf("pairs = %v, want %v", pairs, want)
	}
	wantProblems := []string{"line 7: EMPTY has no value", "line 8: want NAME=value", "line 9: want NAME=value"}
	if !slices.Equal(problems, wantProblems) {
		t.Errorf("problems = %v, want %v", problems, wantProblems)
	}
}

// R4: the legacy providers.ReadEnvFile trimmed whitespace before stripping quotes, so
// `TOKEN= "abc"` resolved to `abc`; ParseDotenv must match or migration silently corrupts values
// (and whitespace around `@keyring` must not block reference conversion either).
func TestParseDotenvTrimsWhitespaceBeforeStrippingQuotes(t *testing.T) {
	in := "TOKEN= \"abc\" \nREF= @keyring \n"
	pairs, problems := ParseDotenv(strings.NewReader(in))
	want := []Pair{{"TOKEN", "abc"}, {"REF", "@keyring"}}
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if !slices.Equal(pairs, want) {
		t.Errorf("pairs = %v, want %v", pairs, want)
	}
}

func TestParseDotenvQuotesAndLongLines(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	in := "S='single'\nM=\"mismatch'\nE=\"\"\nL=" + long + "\nB=" + strings.Repeat("y", 5<<20) + "\nAFTER=lost\n"
	pairs, problems := ParseDotenv(strings.NewReader(in))
	want := []Pair{{"S", "single"}, {"M", `"mismatch'`}, {"L", long}}
	if !slices.Equal(pairs, want) {
		t.Errorf("pairs = %d, want S, M verbatim, a 100 KiB L", len(pairs))
	}
	if len(problems) != 2 || problems[0] != "line 3: E has no value" || !strings.HasPrefix(problems[1], "read stopped early") {
		t.Errorf("problems = %.200v, want E empty then read stopped early", problems)
	}
}

func TestExportPairsStoredOnlyAndRestricted(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", "from-env")
	for _, kv := range [][2]string{{"A_KEY", "a"}, {"B_KEY", "b"}} {
		if err := Store(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("A_KEY", "shadowing-env")
	if got, err := ExportPairs(nil); err != nil || !slices.Equal(got, []Pair{{"A_KEY", "a"}, {"B_KEY", "b"}}) {
		t.Errorf("export all = %v, %v (process env must not leak in)", got, err)
	}
	if got, err := ExportPairs([]string{"B_KEY"}); err != nil || !slices.Equal(got, []Pair{{"B_KEY", "b"}}) {
		t.Errorf("export restricted = %v, %v", got, err)
	}
}

func TestListStoredMasksWithoutDecrypting(t *testing.T) {
	vaulttest.Use(t)
	const secret = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	for name, value := range map[string]string{"Z_KEY": "short", "A_KEY": secret} {
		if err := Store(name, value); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ONLY_IN_ENV", "from-env")
	wrongVaultKey(t)
	want := []Stored{
		{Name: "A_KEY", Source: "vault", Prefix: "sk-test-", Len: len(secret)},
		{Name: "Z_KEY", Source: "vault", Len: 5},
	}
	got, err := ListStored(nil)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListStored with wrong key = %+v, %v, want %+v", got, err, want)
	}
	if got[0].Masked() != "sk-test-*** (44 chars)" || got[1].Masked() != "5 chars" {
		t.Fatalf("Masked = %q, %q", got[0].Masked(), got[1].Masked())
	}
	if got, err := ListStored([]string{"Z_KEY", "ONLY_IN_ENV", "MISSING"}); err != nil || !slices.Equal(got, want[1:]) {
		t.Fatalf("ListStored restricted = %+v, %v", got, err)
	}
	if pairs, err := ExportPairs(nil); !errors.Is(err, vault.ErrWrongKey) || pairs != nil ||
		!strings.Contains(err.Error(), "export: A_KEY:") {
		t.Fatalf("ExportPairs with wrong key = %v, %v, want nil, named ErrWrongKey", pairs, err)
	}
}

func TestExportPairsDiscardsPartialOutput(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "A_GOOD", "first-value")
	vaulttest.Secret(t, "Z_BAD", "second-value")
	if err := vault.Update(func(tx *vault.Tx) error {
		r, _, err := tx.Record("Z_BAD")
		if err != nil {
			return err
		}
		r.Sealed[len(r.Sealed)-1] ^= 1
		return tx.PutRecord("Z_BAD", r)
	}); err != nil {
		t.Fatal(err)
	}
	if pairs, err := ExportPairs(nil); err == nil || pairs != nil || !strings.Contains(err.Error(), "export: Z_BAD:") {
		t.Fatalf("ExportPairs = %v, %v, want nil and named decryption error", pairs, err)
	}
	if pairs, err := ExportPairs([]string{"A_GOOD"}); err != nil || !slices.Equal(pairs, []Pair{{"A_GOOD", "first-value"}}) {
		t.Fatalf("restricted ExportPairs = %v, %v", pairs, err)
	}
}

func TestListStoredAndExportPairsSurfaceMetadataErrors(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Secrets, "BROKEN", "{")
	if rows, err := ListStored(nil); err == nil || rows != nil {
		t.Fatalf("ListStored = %v, %v, want nil, error", rows, err)
	}
	if pairs, err := ExportPairs(nil); err == nil || pairs != nil {
		t.Fatalf("ExportPairs = %v, %v, want nil, error", pairs, err)
	}
}

func TestAppendDotenvCreatesAppendsSkipsAndTightens(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if n, _, err := AppendDotenv(path, []Pair{{"A", "1"}}); err != nil || n != 1 {
		t.Fatalf("create: n=%d err=%v", n, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(must(os.ReadFile(path)), "B=old"...), 0o644); err != nil { // no trailing newline
		t.Fatal(err)
	}
	n, skipped, err := AppendDotenv(path, []Pair{{"A", "x"}, {"B", "y"}, {"C", "3"}})
	if err != nil || n != 1 || !slices.Equal(skipped, []string{"A", "B"}) {
		t.Fatalf("append: n=%d skipped=%v err=%v", n, skipped, err)
	}
	got := string(must(os.ReadFile(path)))
	if !strings.HasPrefix(got, "# ") || !strings.HasSuffix(got, "\nA=1\nB=old\nC=3\n") {
		t.Errorf("file = %q", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
