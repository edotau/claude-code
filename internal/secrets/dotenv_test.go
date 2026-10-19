package secrets

import (
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
	if got := FormatDotenv([]Pair{{"A_KEY", "a"}}); !strings.HasSuffix(got, "\nA_KEY=a\n") || !strings.HasPrefix(got, "# ") {
		t.Errorf("format = %q", got)
	}
}
