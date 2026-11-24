package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestWorkflowTarget(t *testing.T) {
	repo := t.TempDir()
	for _, d := range []string{".git", "workflows", "internal/assets/files/workflows/blocks", "sub"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		name, cwd string
		args      []string
		want      string
	}{
		{"argument wins", filepath.Join(repo, "sub"), []string{"/x/y"}, "/x/y"},
		{"inside checkout", filepath.Join(repo, "sub"), nil, repo},
		{"outside", t.TempDir(), nil, paths.ConfigDir()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Chdir(c.cwd)
			var out bytes.Buffer
			got, err := workflowTarget("lint", c.args, &out)
			if err != nil || len(got) != 1 {
				t.Fatalf("got %v, %v", got, err)
			}
			if got[0] != c.want && !sameDir(t, got[0], c.want) {
				t.Errorf("got %q want %q", got[0], c.want)
			}
			if line, _, _ := strings.Cut(out.String(), "\n"); !strings.HasPrefix(line, "workflow lint: /") {
				t.Errorf("first line %q", line)
			}
		})
	}
}

func TestWorkflowTargetRejects(t *testing.T) {
	for _, bad := range [][]string{{""}, {"-x"}, {"a", "b"}} {
		var out bytes.Buffer
		if _, err := workflowTarget("sync", bad, &out); err == nil || out.Len() != 0 {
			t.Errorf("args %q: err %v, output %q", bad, err, out.String())
		}
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}
