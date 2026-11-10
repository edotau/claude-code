package memorycmd

import (
	"github.com/edotau/claude-code/internal/memory"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

func TestMemoryDreamRejectsStrayBeforeDryRun(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(cfg, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An invalid config directory prevents a regressed forced dream from reaching the model runner.
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir())
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"dream", "--force", "stray", "--dry-run"}, nil)
	})
	if code != 2 {
		t.Errorf("dream ignored stray before --dry-run: exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, `memory dream: unexpected argument "stray"`) {
		t.Errorf("dream missing positional usage error: %q", stderr)
	}
	if out != "" {
		t.Errorf("dream ran despite invalid arguments: %q", out)
	}
}

func TestMemoryFmtRejectsStrayBeforeCheck(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Chdir(t.TempDir())
	dir := memory.HomeBankDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "decisionLog.md")
	before := "# Decision Log\n\n- " + strings.Repeat("longword ", 30) + "end\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"fmt", "stray", "--check"}, nil)
	})
	if code != 2 {
		t.Errorf("fmt ignored stray before --check: exit=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if !strings.Contains(stderr, `memory fmt: unexpected argument "stray"`) {
		t.Errorf("fmt missing positional usage error: %q", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("fmt wrote bank file despite invalid arguments: %q", after)
	}
}

func TestMemoryCompletionSupportedCommandsAndFlags(t *testing.T) {
	engine := completion.Engine{
		Tree:    func() map[string]*completion.Node { return map[string]*completion.Node{"memory": Spec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
	cases := []struct {
		name  string
		words []string
		want  []string
	}{
		{"verify command", []string{"memory", "ver"}, []string{"verify"}},
		{"verify flags", []string{"memory", "verify", "--"}, []string{"--cwd", "--archives", "--all-stores"}},
		{"review flags", []string{"memory", "review", "--"}, []string{"--candidates"}},
		{"update flags", []string{"memory", "update", "--"}, []string{"--dry-run", "--historical"}},
		{"dream flags", []string{"memory", "dream", "--"}, []string{"--dry-run"}},
		{"catchup flags", []string{"memory", "catchup", "--"}, []string{"--dry-run", "--skip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.Candidates(tc.words)
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("completion for %v missing %q: %v", tc.words, want, got)
				}
			}
		})
	}
}

func TestMemoryPathRepoFlagResolvesStoreName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gittest.Repo(t)
	dir, err := memory.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(repo)
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"path", "--repo", name}, nil) })
	if code != 0 || strings.TrimSpace(out) != dir {
		t.Errorf("memory path --repo %s = %d %q (stderr %q), want %q", name, code, out, stderr, dir)
	}
}

func TestMemoryCompletionOffersIndexAndRecallFlags(t *testing.T) {
	engine := completion.Engine{
		Tree:    func() map[string]*completion.Node { return map[string]*completion.Node{"memory": Spec()} },
		Resolve: func(verb string) (string, bool) { return verb, verb == "memory" },
	}
	for verb, want := range map[string][]string{"index": {"--reembed", "--mint-only"}, "recall": {"--explain"}} {
		got := engine.Candidates([]string{"memory", verb, "--"})
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("memory %s completion missing %q: %v", verb, w, got)
			}
		}
	}
}
