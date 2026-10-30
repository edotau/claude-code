package hooks

import (
	"path/filepath"
	"testing"
)

// TestHarnessGitBlocked pins harnessGitBlocked's current behavior directly (gocognit refactor-risk,
// internal/hooks/safety.go:190): banned verbs inside the config tree, the worktree exception, -C/
// --work-tree/--git-dir target resolution, cd tracking, substitutions and nested shells.
func TestHarnessGitBlocked(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	outside := t.TempDir()
	worktree := filepath.Join(cfg, ".claude", "worktrees", "repo", "slug")

	cases := []struct {
		name string
		cmd  string
		cwd  string
		want bool
	}{
		{"stash inside tree", "git stash", cfg, true},
		{"checkout inside tree", "git checkout main", cfg, true},
		{"reset inside tree", "git reset --hard HEAD", cfg, true},
		{"restore inside tree", "git restore .", cfg, true},
		{"clean -f inside tree", "git clean -f", cfg, true},
		{"commit inside tree allowed", "git commit -m x", cfg, false},
		{"push inside tree allowed", "git push", cfg, false},
		{"stash outside tree", "git stash", outside, false},
		{"stash list inside tree allowed", "git stash list", cfg, false},
		{"stash show inside tree allowed", "git stash show", cfg, false},
		{"stash pop inside tree blocked", "git stash pop", cfg, true},
		{"worktree exception", "git reset --hard HEAD", worktree, false},
		{"-C into tree from outside cwd", "git -C " + cfg + " reset --hard", outside, true},
		{"--work-tree into tree", "git --work-tree=" + cfg + " --git-dir=" + cfg + "/.git reset --hard", outside, true},
		{"cd into tree then reset", "cd " + cfg + " && git reset --hard", outside, true},
		{"cd out of tree then reset", "cd " + outside + " && git reset --hard", cfg, false},
		{"command substitution into tree", "echo $(cd " + cfg + " && git reset --hard)", outside, true},
		{"nested bash -c into tree", `bash -c "git reset --hard"`, cfg, true},
		{"nested bash -c outside tree", `bash -c "git reset --hard"`, outside, false},
		{"eval into tree", "eval git reset --hard", cfg, true},
		{"non-git command", "ls -la", cfg, false},
		{"ssh command runs remotely", "ssh wks git reset --hard", cfg, false},
		{"ssh heredoc runs remotely", "ssh wks 'bash -s' > log 2>&1 <<'EOF'\ngit checkout -q HEAD\nEOF", cfg, false},
		{"local git after ssh heredoc", "ssh wks <<'EOF'\nuptime\nEOF\ngit reset --hard", cfg, true},
		{"bash heredoc stays local", "bash -s <<'EOF'\ngit reset --hard\nEOF", cfg, true},
		{"ssh heredoc piped to a local shell", "ssh wks <<'EOF' | bash\ngit reset --hard\nEOF", cfg, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := harnessGitBlocked(tt.cmd, tt.cwd); got != tt.want {
				t.Errorf("harnessGitBlocked(%q, %q) = %v, want %v", tt.cmd, tt.cwd, got, tt.want)
			}
		})
	}
}
