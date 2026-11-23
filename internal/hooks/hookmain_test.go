package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestHookMainRejectsStaleMainStamp(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, paths.StampFile)
	main := filepath.Join(dir, "claude-code")
	old := paths.BuildStamp
	paths.BuildStamp = "same-stamp"
	t.Cleanup(func() { paths.BuildStamp = old })
	mtime := time.Unix(1_700_000_000, 0)
	valid := fmt.Sprintf("%s %d %d\n", paths.BuildStamp, len("main"), mtime.Unix())
	for _, tc := range []struct {
		name, body, stamp string
		mtime             time.Time
		want              bool
	}{
		{"matching", "main", valid, mtime, true},
		{"preserved mtime", "replacement main", valid, mtime, false},
		{"older mtime", "main", valid, mtime.Add(-time.Hour), false},
		{"newer mtime", "main", valid, mtime.Add(time.Hour), false},
		{"legacy", "main", paths.BuildStamp, mtime, false},
		{"wrong build", "main", "other " + valid, mtime, false},
		{"malformed", "main", paths.BuildStamp + " size mtime", mtime, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(main, []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(main, tc.mtime, tc.mtime); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stamp, []byte(tc.stamp), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := stampMatches(dir); got != tc.want {
				t.Fatalf("stampMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHookMainFallsBackWhenMainCannotExecute(t *testing.T) {
	main := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(main, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mainSibling
	mainSibling = func() (string, bool) { return main, true }
	t.Cleanup(func() { mainSibling = old })
	if got := Main([]string{paths.HookBinaryName, "PreToolUse", "--phase69-probe"}); got != 1 {
		t.Fatalf("failed hand-over returned %d, want local unknown-module exit 1", got)
	}
}
