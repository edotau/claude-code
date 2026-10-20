package secrets

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseDotenvStrictFormat(t *testing.T) {
	in := "# comment\nENVIRONMENT=dev\n\nURL=https://x?a=b\nexport FOO=bar\nQUOTED=\"kept as is\"\nEMPTY=\n9BAD=x\nno equals\n"
	pairs, problems := ParseDotenv(strings.NewReader(in))
	want := []Pair{{"ENVIRONMENT", "dev"}, {"URL", "https://x?a=b"}, {"QUOTED", `"kept as is"`}}
	if !slices.Equal(pairs, want) {
		t.Errorf("pairs = %v, want %v", pairs, want)
	}
	wantProblems := []string{"line 5: want NAME=value", "line 7: EMPTY has no value", "line 8: want NAME=value", "line 9: want NAME=value"}
	if !slices.Equal(problems, wantProblems) {
		t.Errorf("problems = %v, want %v", problems, wantProblems)
	}
}

func TestExportPairsStoredOnlyAndRestricted(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	t.Setenv("ONLY_IN_ENV", "from-env")
	for _, kv := range [][2]string{{"A_KEY", "a"}, {"B_KEY", "b"}} {
		if _, err := Store(kv[0], kv[1], true); err != nil {
			t.Fatal(err)
		}
	}
	if got := ExportPairs(nil, nil); !slices.Equal(got, []Pair{{"A_KEY", "a"}, {"B_KEY", "b"}}) {
		t.Errorf("export all = %v (process env must not leak in)", got)
	}
	if got := ExportPairs(nil, []string{"B_KEY"}); !slices.Equal(got, []Pair{{"B_KEY", "b"}}) {
		t.Errorf("export restricted = %v", got)
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
