package launch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCodexFileOverrides flattens top-level keys and nested tables into -c pairs and refuses multi-line values.
func TestCodexFileOverrides(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	os.WriteFile(good, []byte("# c\nmodel = \"m\"\n\n[model_providers.d]\nbase_url = \"https://h\"\n[model_providers.d.auth]\nargs = [\"resolve\", \"--provider\", \"qa\"]\n"), 0o600)
	got, err := codexFileOverrides(good)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", `model="m"`, "-c", `model_providers.d.base_url="https://h"`,
		"-c", `model_providers.d.auth.args=["resolve", "--provider", "qa"]`}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("args = [\n  \"x\",\n]\n"), 0o600)
	if _, err := codexFileOverrides(bad); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Errorf("multi-line value: err = %v, want a line-1 error", err)
	}
}

// TestParseArgsFileFlag: -f/--file is taken on vendor launches only.
func TestParseArgsFileFlag(t *testing.T) {
	for _, args := range [][]string{{"-f", "x.toml", "exec"}, {"--file=x.toml", "exec"}} {
		o, rest, err := ParseArgs(args, false)
		if err != nil || o.File != "x.toml" || !slices.Equal(rest, []string{"exec"}) {
			t.Errorf("ParseArgs(%q) = %+v, %q, %v", args, o, rest, err)
		}
	}
	if o, rest, _ := ParseArgs([]string{"-f", "x"}, true); o.File != "" || len(rest) != 2 {
		t.Errorf("Claude launch must pass -f through: %+v %q", o, rest)
	}
}
