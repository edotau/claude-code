package vault_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestParseDotenvStrictFormat(t *testing.T) {
	in := "# comment\nENVIRONMENT=dev\n\nURL=https://x?a=b\nexport FOO=bar\nQUOTED=\"kept as is\"\nEMPTY=\n9BAD=x\nno equals\n"
	pairs, problems := vault.ParseDotenv(strings.NewReader(in))
	want := []vault.Pair{{"ENVIRONMENT", "dev"}, {"URL", "https://x?a=b"}, {"FOO", "bar"}, {"QUOTED", "kept as is"}}
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
	pairs, problems := vault.ParseDotenv(strings.NewReader(in))
	want := []vault.Pair{{"TOKEN", "abc"}, {"REF", "@keyring"}}
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
	pairs, problems := vault.ParseDotenv(strings.NewReader(in))
	want := []vault.Pair{{"S", "single"}, {"M", `"mismatch'`}, {"L", long}}
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
		if err := vault.Store(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("A_KEY", "shadowing-env")
	if got, err := vault.ExportPairs(nil); err != nil || !slices.Equal(got, []vault.Pair{{"A_KEY", "a"}, {"B_KEY", "b"}}) {
		t.Errorf("export all = %v, %v (process env must not leak in)", got, err)
	}
	if got, err := vault.ExportPairs([]string{"B_KEY"}); err != nil || !slices.Equal(got, []vault.Pair{{"B_KEY", "b"}}) {
		t.Errorf("export restricted = %v, %v", got, err)
	}
}

func TestListStoredMasksWithoutDecrypting(t *testing.T) {
	vaulttest.Use(t)
	const secret = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	storeAll(t, map[string]string{"Z_KEY": "short", "A_KEY": secret})
	t.Setenv("ONLY_IN_ENV", "from-env")
	wrongVaultKey(t)
	want := []vault.Stored{
		{Name: "A_KEY", Prefix: "sk-test-", Len: len(secret)},
		{Name: "Z_KEY", Len: 5},
	}
	got, err := vault.ListStored(nil)
	zeroSetAt(t, got)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListStored with wrong key = %+v, %v, want %+v", got, err, want)
	}
	if got[0].Masked() != "sk-test-*** (44 chars)" || got[1].Masked() != "5 chars" {
		t.Fatalf("Masked = %q, %q", got[0].Masked(), got[1].Masked())
	}
	got, err = vault.ListStored([]string{"Z_KEY", "ONLY_IN_ENV", "MISSING"})
	if len(got) == 1 {
		got[0].SetAt = time.Time{}
	}
	if err != nil || !slices.Equal(got, want[1:]) {
		t.Fatalf("ListStored restricted = %+v, %v", got, err)
	}
	checkExportWrongKey(t)
}

// storeAll seals every name=value in kv into the test vault.
func storeAll(t *testing.T, kv map[string]string) {
	t.Helper()
	for name, value := range kv {
		if err := vault.Store(name, value); err != nil {
			t.Fatal(err)
		}
	}
}

// zeroSetAt fails on any row without a seal time, then zeroes SetAt so the rest compares exactly.
func zeroSetAt(t *testing.T, got []vault.Stored) {
	t.Helper()
	for i := range got {
		if got[i].SetAt.IsZero() {
			t.Errorf("%s: SetAt is zero, want the record's seal time", got[i].Name)
		}
		got[i].SetAt = time.Time{} // compared above; the rest is exact
	}
}

// checkExportWrongKey asserts ExportPairs under a wrong key returns no pairs and an ErrWrongKey naming A_KEY.
func checkExportWrongKey(t *testing.T) {
	t.Helper()
	if pairs, err := vault.ExportPairs(nil); !errors.Is(err, vault.ErrWrongKey) || pairs != nil ||
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
	if pairs, err := vault.ExportPairs(nil); err == nil || pairs != nil || !strings.Contains(err.Error(), "export: Z_BAD:") {
		t.Fatalf("ExportPairs = %v, %v, want nil and named decryption error", pairs, err)
	}
	if pairs, err := vault.ExportPairs([]string{"A_GOOD"}); err != nil || !slices.Equal(pairs, []vault.Pair{{"A_GOOD", "first-value"}}) {
		t.Fatalf("restricted vault.ExportPairs = %v, %v", pairs, err)
	}
}

func TestListStoredAndExportPairsSurfaceMetadataErrors(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Secrets, "BROKEN", "{")
	if rows, err := vault.ListStored(nil); err == nil || rows != nil {
		t.Fatalf("ListStored = %v, %v, want nil, error", rows, err)
	}
	if pairs, err := vault.ExportPairs(nil); err == nil || pairs != nil {
		t.Fatalf("ExportPairs = %v, %v, want nil, error", pairs, err)
	}
}

func TestAppendDotenvCreatesAppendsSkipsAndTightens(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if n, _, err := vault.AppendDotenv(path, []vault.Pair{{"A", "1"}}); err != nil || n != 1 {
		t.Fatalf("create: n=%d err=%v", n, err)
	}
	loosenAndAppend(t, path, "B=old") // no trailing newline
	n, skipped, err := vault.AppendDotenv(path, []vault.Pair{{"A", "x"}, {"B", "y"}, {"C", "3"}})
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

// loosenAndAppend makes path 0644 and appends tail verbatim, as a hand-edited .env would be.
func loosenAndAppend(t *testing.T, path, tail string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(must(os.ReadFile(path)), tail...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// AppendDotenv must quote any value ParseDotenv would otherwise mis-parse, and ParseDotenv must
// recover the exact original value back out — including a value that is itself quote-bounded.
func TestAppendDotenvParseDotenvRoundTripsTrickyValues(t *testing.T) {
	tricky := []vault.Pair{
		{"SPACE", "has a space"},
		{"HASH", "trailing # not a comment"},
		{"DOLLAR", "price is $5"},
		{"SINGLE", "it's mine"},
		{"DOUBLE", `say "hi"`},
		// These two need the quoting: left raw, ParseDotenv's unconditional "strip one matching
		// outer quote pair" would eat the value's own quotes, and TrimSpace would eat its padding.
		{"ALREADY_SINGLE_BOUNDED", "'quoted value'"},
		{"ALREADY_DOUBLE_BOUNDED", `"quoted value"`},
		{"LEADING_SPACE", " leading space"},
		{"TRAILING_SPACE", "trailing space "},
	}
	path := filepath.Join(t.TempDir(), ".env")
	n, skipped, err := vault.AppendDotenv(path, tricky)
	if err != nil || n != len(tricky) || len(skipped) != 0 {
		t.Fatalf("AppendDotenv = %d, %v, %v", n, skipped, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pairs, problems := vault.ParseDotenv(strings.NewReader(string(raw)))
	if len(problems) != 0 {
		t.Fatalf("ParseDotenv problems = %v on:\n%s", problems, raw)
	}
	if !slices.Equal(pairs, tricky) {
		t.Fatalf("round-trip = %v, want %v\nfile:\n%s", pairs, tricky, raw)
	}
}

// AppendDotenv must refuse to write through a symlink rather than follow it to an arbitrary target.
func TestAppendDotenvRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("EXISTING=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.AppendDotenv(link, []vault.Pair{{"A", "1"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("AppendDotenv through a symlink = %v, want a symlink refusal", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "EXISTING=1\n" {
		t.Fatalf("AppendDotenv wrote through the symlink: %q", got)
	}
}

// AppendDotenv warns on stderr when it writes the default ./.env in a git work tree that does not
// gitignore it; a non-default path, or one already ignored, must stay silent.
func TestAppendDotenvWarnsWhenDefaultEnvNotGitignored(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err := os.Stat("/opt/homebrew/bin/git"); err != nil {
			t.Skip("git not found")
		}
	}
	repo := t.TempDir()
	t.Chdir(repo)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")

	out := captureStderr(t, func() {
		if _, _, err := vault.AppendDotenv(".env", []vault.Pair{{"A", "1"}}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, ".env") || !strings.Contains(out, "gitignore") {
		t.Fatalf("stderr = %q, want a gitignore warning naming .env", out)
	}

	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = captureStderr(t, func() {
		if _, _, err := vault.AppendDotenv(".env", []vault.Pair{{"B", "2"}}); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("stderr = %q, want silence once .env is gitignored", out)
	}
}

// warnIfUntrackedDefault must stay silent outside a git work tree: `git check-ignore` there exits
// 128, not 1, and only exit 1 ("tracked path, not ignored") is a real warning case.
func TestWarnIfUntrackedDefaultOutsideRepo(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		if _, err := os.Stat("/opt/homebrew/bin/git"); err != nil {
			t.Skip("git not found")
		}
	}
	dir := t.TempDir()
	t.Chdir(dir)

	out := captureStderr(t, func() {
		if _, _, err := vault.AppendDotenv(".env", []vault.Pair{{"A", "1"}}); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("stderr = %q, want silence outside a git work tree", out)
	}
}

func TestParseDotenvSkipsIndentedComment(t *testing.T) {
	pairs, problems := vault.ParseDotenv(strings.NewReader("  # note\n\t# tab\nA=1\n"))
	if len(problems) != 0 || !slices.Equal(pairs, []vault.Pair{{"A", "1"}}) {
		t.Errorf("pairs = %v, problems = %v; want only A=1 and no problems", pairs, problems)
	}
}
