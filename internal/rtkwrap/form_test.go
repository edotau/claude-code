package rtkwrap

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func TestForModel(t *testing.T) {
	bin := hosttest.FakeRTK(t)
	onPath := filepath.Dir(bin) + string(os.PathListSeparator) + os.Getenv("PATH")
	cwd, ctx := t.TempDir(), context.Background()
	for _, tc := range []struct{ name, cmd, codemode, path, want string }{
		{"rewrites", "git log -3", "", onPath, "rtk git log -3"},
		{"code mode", "git log -3", "on", onPath, "git log -3"},
		{"veto", "go vet ./x", "", onPath, "go vet ./x"},
		{"already rtk", "rtk git status", "", onPath, "rtk git status"},
		{"bare rtk off PATH", "git log -4", "", "/nonexistent", "git log -4"},
	} {
		t.Setenv(CodemodeEnv, tc.codemode)
		t.Setenv("PATH", tc.path)
		if got := ForModel(ctx, tc.cmd, cwd); got != tc.want {
			t.Errorf("%s: ForModel(%q) = %q, want %q", tc.name, tc.cmd, got, tc.want)
		}
	}
	t.Setenv("PATH", onPath)
	if got := ArgvForModel(ctx, []string{"git", "log", "-5"}, cwd); !slices.Equal(got, []string{bin, "git", "log", "-5"}) {
		t.Errorf("ArgvForModel = %q", got)
	}
	if got := ArgvForModel(ctx, []string{"go", "vet", "./x"}, cwd); !slices.Equal(got, []string{"go", "vet", "./x"}) {
		t.Errorf("ArgvForModel(veto) = %q", got)
	}
	t.Setenv(paths.RTKBinaryEnv, "/nonexistent/rtk")
	if got := ForModel(ctx, "git log -6", cwd); got != "git log -6" {
		t.Errorf("no rtk: ForModel = %q", got)
	}
}

// forModelSites are the only files that may call ForModel or ArgvForModel: the reviewed model-read inventory.
var forModelSites = []string{
	"internal/agent/waves/verify.go",
	"internal/copilot/sdk/rtk.go",
	"internal/review/bench/driver.go",
	"internal/worktree/gate.go",
}

// TestForModelCallSitesAllowlisted keeps the helper off program-parsed output: a new caller joins the list on review.
func TestForModelCallSitesAllowlisted(t *testing.T) {
	root := filepath.Join("..", "..")
	call := regexp.MustCompile(`\brtkwrap\.(ArgvForModel|ForModel)\(`)
	var got []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err == nil && call.Match(b) {
				rel, _ := filepath.Rel(root, p)
				got = append(got, filepath.ToSlash(rel))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, forModelSites) {
		t.Errorf("ForModel callers = %v, want %v", got, forModelSites)
	}
}
