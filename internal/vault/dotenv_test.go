package vault

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// ParseDotenv keeps NAME=value lines, trims before stripping one quote pair (R4) and reports malformed lines.
func TestParseDotenv(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		pairs    []Pair
		problems []string
	}{
		{"strict format",
			"# comment\nENVIRONMENT=dev\n\nURL=https://x?a=b\nexport FOO=bar\nQUOTED=\"kept as is\"\nEMPTY=\n9BAD=x\nno equals\n",
			[]Pair{{"ENVIRONMENT", "dev"}, {"URL", "https://x?a=b"}, {"FOO", "bar"}, {"QUOTED", "kept as is"}},
			[]string{"line 7: EMPTY has no value", "line 8: want NAME=value", "line 9: want NAME=value"}},
		{"trims whitespace before stripping quotes", "TOKEN= \"abc\" \nREF= @keyring \n",
			[]Pair{{"TOKEN", "abc"}, {"REF", "@keyring"}}, nil},
		{"indented comment", "  # note\n\t# tab\nA=1\n", []Pair{{"A", "1"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pairs, problems := ParseDotenv(strings.NewReader(tc.in))
			if !slices.Equal(pairs, tc.pairs) || !slices.Equal(problems, tc.problems) {
				t.Errorf("ParseDotenv = %v, %v; want %v, %v", pairs, problems, tc.pairs, tc.problems)
			}
		})
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
	useZeroRootVault(t)
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
	useZeroRootVault(t)
	const secret = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	storeAll(t, map[string]string{"Z_KEY": "short", "A_KEY": secret})
	t.Setenv("ONLY_IN_ENV", "from-env")
	useWrongVaultKey(t)
	want := []Stored{
		{Name: "A_KEY", Prefix: "sk-test-", Len: len(secret)},
		{Name: "Z_KEY", Len: 5},
	}
	got, err := ListStored(nil)
	zeroSetAt(t, got)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListStored with wrong key = %+v, %v, want %+v", got, err, want)
	}
	if got[0].Masked() != "sk-test-*** (44 chars)" || got[1].Masked() != "5 chars" {
		t.Fatalf("Masked = %q, %q", got[0].Masked(), got[1].Masked())
	}
	got, err = ListStored([]string{"Z_KEY", "ONLY_IN_ENV", "MISSING"})
	if len(got) == 1 {
		got[0].SetAt = time.Time{}
	}
	if err != nil || !slices.Equal(got, want[1:]) {
		t.Fatalf("ListStored restricted = %+v, %v", got, err)
	}
	checkExportWrongKey(t)
}

// storeAll seals every name=value in kv into the test
func storeAll(t *testing.T, kv map[string]string) {
	t.Helper()
	for name, value := range kv {
		if err := Store(name, value); err != nil {
			t.Fatal(err)
		}
	}
}

// zeroSetAt fails on any row without a seal time, then zeroes SetAt so the rest compares exactly.
func zeroSetAt(t *testing.T, got []Stored) {
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
	if pairs, err := ExportPairs(nil); !errors.Is(err, ErrWrongKey) || pairs != nil ||
		!strings.Contains(err.Error(), "export: A_KEY:") {
		t.Fatalf("ExportPairs with wrong key = %v, %v, want nil, named ErrWrongKey", pairs, err)
	}
}

func TestExportPairsDiscardsPartialOutput(t *testing.T) {
	useZeroRootVault(t)
	putTestSecret(t, "A_GOOD", "first-value")
	putTestSecret(t, "Z_BAD", "second-value")
	if err := Update(func(tx *Tx) error {
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
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.Put(Secrets, "BROKEN", "{") }); err != nil {
		t.Fatal(err)
	}
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
	loosenAndAppend(t, path, "B=old") // no trailing newline
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

// captureStderr returns what fn wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	_, _, stderr := hosttest.CaptureOutput(t, func() int { fn(); return 0 })
	return stderr
}

// AppendDotenv must quote any value ParseDotenv would otherwise mis-parse, and ParseDotenv must
// recover the exact original value back out — including a value that is itself quote-bounded.
func TestAppendDotenvParseDotenvRoundTripsTrickyValues(t *testing.T) {
	tricky := []Pair{
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
	n, skipped, err := AppendDotenv(path, tricky)
	if err != nil || n != len(tricky) || len(skipped) != 0 {
		t.Fatalf("AppendDotenv = %d, %v, %v", n, skipped, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pairs, problems := ParseDotenv(strings.NewReader(string(raw)))
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
	if _, _, err := AppendDotenv(link, []Pair{{"A", "1"}}); err == nil || !strings.Contains(err.Error(), "symlink") {
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
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
		if _, _, err := AppendDotenv(".env", []Pair{{"A", "1"}}); err != nil {
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
		if _, _, err := AppendDotenv(".env", []Pair{{"B", "2"}}); err != nil {
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	dir := t.TempDir()
	t.Chdir(dir)

	out := captureStderr(t, func() {
		if _, _, err := AppendDotenv(".env", []Pair{{"A", "1"}}); err != nil {
			t.Fatal(err)
		}
	})
	if out != "" {
		t.Fatalf("stderr = %q, want silence outside a git work tree", out)
	}
}

// A failed partial append is truncated back to the original bytes, so a retry writes the name instead of skipping it.
func TestAppendDotenvRollsBackPartialWrite(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), ".env")
	const original = "KEEP=yes\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	pair := Pair{Name: "TOKEN", Value: strings.Repeat("abcdef", 10)}
	orig := dotenvWrite
	dotenvWrite = func(f *os.File, s string) (int, error) {
		n, err := f.WriteString(s[:len(s)/2])
		return n, errors.Join(err, io.ErrShortWrite)
	}
	restore := func() { dotenvWrite = orig }
	n, skipped, err := AppendDotenv(path, []Pair{pair})
	restore()
	if n != 0 || len(skipped) != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial append = %d, %v, %v; want a failed partial write", n, skipped, err)
	}
	checkDotenvContents(t, path, original)
	n, skipped, err = AppendDotenv(path, []Pair{pair})
	if err != nil || n != 1 || len(skipped) != 0 {
		t.Fatalf("retry = %d, %v, %v; want TOKEN written, not skipped", n, skipped, err)
	}
	checkDotenvContents(t, path, original+"TOKEN="+pair.Value+"\n")
}

func checkDotenvContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("dotenv contents = %q, want %q", got, want)
	}
}

// putTestSecret seals value under name with the test vault's key.
func putTestSecret(t *testing.T, name, value string) {
	t.Helper()
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Sealed: k.Seal(name, value), Kid: k.Kid, Len: len(value), SetAt: time.Now()}
	if err = Update(func(tx *Tx) error { return tx.PutRecord(name, r) }); err != nil {
		t.Fatal(err)
	}
}

// useWrongVaultKey points the vault at a different master key with an empty fake keyring.
func useWrongVaultKey(t *testing.T) {
	t.Helper()
	t.Cleanup(UseKeyringSeams(func(string) (string, bool, error) { return "", false, nil },
		func(string, string) error { return nil }))
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

// A multi-line value cannot round-trip through a .env line, so AppendDotenv refuses it and writes nothing.
func TestAppendDotenvRefusesMultilineValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	n, _, err := AppendDotenv(path, []Pair{{"OK", "fine"}, {"PEM", "-----BEGIN\nabc\n-----END"}})
	if err == nil || !strings.Contains(err.Error(), "PEM") || n != 0 {
		t.Fatalf("AppendDotenv = %d, %v; want a PEM newline error and 0 added", n, err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("a refused export left %s behind (stat err %v)", path, serr)
	}
}
