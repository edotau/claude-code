package mdlint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapItem(t *testing.T) {
	span := "`" + strings.Repeat("x y ", 9) + "end`"
	line := "- [ ] " + strings.Repeat("word ", 15) + span + " " + strings.Repeat("tail ", 20) + "last"
	got := WrapItem(line)
	if len(got) < 2 {
		t.Fatalf("not wrapped: %q", got)
	}
	for i, l := range got {
		if len([]rune(l)) > Width {
			t.Errorf("line %d is %d columns", i, len([]rune(l)))
		}
		if i > 0 && !strings.HasPrefix(l, "      ") {
			t.Errorf("line %d lacks 6-space indent: %q", i, l)
		}
	}
	if !strings.Contains(strings.Join(got, "\n"), span) {
		t.Error("code span was split")
	}
	if a, b := strings.Fields(line), strings.Fields(strings.Join(got, " ")); strings.Join(a, " ") != strings.Join(b, " ") {
		t.Error("words not restored by joining")
	}
}

func TestStoreRules(t *testing.T) {
	if got := string(StoreRules("memory")); !strings.Contains(got, `"extends": "../.markdownlint.json"`) ||
		!strings.Contains(got, `"MD046": false`) {
		t.Fatalf("rules = %s", got)
	}
}

func TestFixFileRootBinNoBuildAndFailure(t *testing.T) {
	root, cfg := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, dir := range []string{filepath.Join(root, "bin"), filepath.Join(cfg, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	argsFile := filepath.Join(root, "args")
	script := "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\necho unfixed report\nexit 1\n"
	fake := filepath.Join(root, "bin", "mdsmith")
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "bin", "mdsmith"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Mdsmith(root); got != fake {
		t.Fatalf("Mdsmith = %q, want %q", got, fake)
	}
	if err := FixFile(context.Background(), root, "a.md"); err == nil {
		t.Fatal("FixFile hid a nonzero exit that printed to stdout")
	}
	if b, _ := os.ReadFile(argsFile); !strings.Contains(string(b), "--no-build") {
		t.Fatalf("fix args = %q, want --no-build", b)
	}
}
