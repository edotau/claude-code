package mdlint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, MdsmithFile), []byte("rules:\n  line-length:\n    max: 120\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fs, err := CheckText(ctx, root, "# T\n\n"+strings.TrimSpace(strings.Repeat("word ", 26))+"\n")
	if errors.Is(err, ErrNoMdsmith) {
		t.Skip("mdsmith not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Rule != "MDS001" {
		t.Fatalf("long line findings = %v", fs)
	}
	if fs, err = CheckText(ctx, root, "# T\n\nshort\n"); err != nil || len(fs) != 0 {
		t.Fatalf("clean text: %v %v", fs, err)
	}
}

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
	dir := t.TempDir()
	for range 2 {
		if err := WriteStoreConfig(dir); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, RulesFile)); string(b) != string(StoreRules("memory")) {
		t.Fatalf("written = %s", b)
	}
}
