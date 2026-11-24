package rtkwrap

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

func TestForModel(t *testing.T) {
	bin := hosttest.FakeRTK(t)
	onPath := filepath.Dir(bin) + string(os.PathListSeparator) + os.Getenv("PATH")
	cwd, ctx, word := t.TempDir(), context.Background(), textutil.QuoteSH(bin)
	for _, tc := range []struct{ name, cmd, codemode, path, want string }{
		{"rewrites", "git log -3", "", onPath, word + " git log -3"},
		{"code mode", "git log -3", "on", onPath, "git log -3"},
		{"veto", "go vet ./x", "", onPath, "go vet ./x"},
		{"already rtk", "rtk git status", "", onPath, "rtk git status"},
		{"pinned off PATH", "git log -4", "", "/nonexistent", word + " git log -4"},
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

// TestForModelCacheSkipsFailuresAndTracksConfig: a failed rewrite is retried and a config edit re-asks rtk.
func TestForModelCacheSkipsFailuresAndTracksConfig(t *testing.T) {
	bin := hosttest.FakeRTK(t)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(CodemodeEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, ctx := t.TempDir(), context.Background()
	t.Setenv("FAKE_RTK_EXIT", "9")
	if got := ForModel(ctx, "git log -7", cwd); got != "git log -7" {
		t.Fatalf("failing rtk: ForModel = %q", got)
	}
	t.Setenv("FAKE_RTK_EXIT", "")
	if got := ForModel(ctx, "git log -7", cwd); !strings.HasSuffix(got, "rtk git log -7") {
		t.Errorf("after a failure: ForModel = %q, want a rewrite", got)
	}
	t.Setenv("FAKE_RTK_OUT", "rtk git log -8 --first")
	if got := ForModel(ctx, "git log -8", cwd); !strings.HasSuffix(got, "--first") {
		t.Fatalf("first form = %q", got)
	}
	config := filepath.Join(home, ".config", "rtk", "config.toml")
	if runtime.GOOS == "darwin" {
		config = filepath.Join(home, "Library", "Application Support", "rtk", "config.toml")
	}
	hosttest.WriteRel(t, filepath.Dir(config), filepath.Base(config), "[hooks]\n")
	t.Setenv("FAKE_RTK_OUT", "rtk git log -8 --second")
	if got := ForModel(ctx, "git log -8", cwd); !strings.HasSuffix(got, "--second") {
		t.Errorf("after a config edit: ForModel = %q, want the new form", got)
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
