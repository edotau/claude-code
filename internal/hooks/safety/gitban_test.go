package safety

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
)

// Hard rule 4: a git tree-write hidden inside an executed heredoc, with cwd inside the harness tree.
func TestSafetyScanChecksHeredocGitInHarness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	cases := []struct {
		cmd  string
		want core.Decision
	}{
		{"bash <<EOF\ngit reset --hard\nEOF", core.Block},
		{pyStdin("import subprocess\nsubprocess.run(['git', 'checkout', 'main'])"), core.Block},
		{pyStdin("from subprocess import call\ncall(('git', 'stash'))"), core.Block},
		{pyStdin("import subprocess\nsubprocess.check_output(['git', 'restore', 'x.txt'])"), core.Block},
		{pyStdin("import subprocess\nsubprocess.run('git clean -fd', shell=True)"), core.Block},
		{pyStdin("import os\nos.execvp('git', ['git', 'reset', '--hard'])"), core.Block},
		{`python3 -c "import subprocess; subprocess.run(['git', 'stash'])"; cd /tmp`, core.Block},
		{"poetry run python - <<'PY'\nimport subprocess\nsubprocess.call(['git', 'stash'])\nPY", core.Block},
		{pyStdin("import subprocess\nsubprocess.run(['git', 'checkout', '-b', 'new'])"), core.Allow},
		{pyStdin("import subprocess\nverb = 'stash'\nsubprocess.run(['git', verb])"), core.Allow},
		{"cd /tmp && " + pyStdin("import subprocess\nsubprocess.run(['git', 'stash'])"), core.Allow},
		{`cd "$(mktemp -d)" && python3 -c "import subprocess; subprocess.run(['git', 'stash'])"`, core.Allow},
		{pyStdin("import subprocess\nsubprocess.run(['git stash'], shell=True)"), core.Block},
		{`cd "$(mktemp -d)" && python3 -c "import os; os.system('git status')" && git stash`, core.Allow},
		{pyStdin("import subprocess\nsubprocess.run(['git', 'checkout', 'main'], cwd='/private/tmp/fx')"), core.Allow},
		{pyStdin("import subprocess\nsubprocess.run(['git', 'checkout', 'main'], cwd='" + home + "')"), core.Block},
		{pyStdin("import os, subprocess\nos.chdir('/private/tmp/fx')\nsubprocess.run(['git', 'reset', '--hard'])"), core.Allow},
		{pyStdin("import subprocess, tempfile\nd = tempfile.mkdtemp()\nsubprocess.run(['git', 'reset', '--hard'], cwd=d)"), core.Allow},
		{pyStdin("import subprocess\nd = os.getcwd()\nsubprocess.run(['git', 'reset', '--hard'], cwd=d)"), core.Block},
		{pyStdin("import subprocess\nsubprocess.run(['git', 'reset', '--hard'])"), core.Block},
	}
	for _, c := range cases {
		payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": home, "tool_input": map[string]string{"command": c.cmd}})
		if got, stderr := safetyScanDecision(t, bytes.NewReader(payload)); got != c.want {
			t.Errorf("%q: got %d want %d (%s)", c.cmd, got, c.want, stderr)
		}
	}
}

// sharedGitFormCases: forms that discard or hide no work pass in the shared tree; the rest and look-alikes block.
func sharedGitFormCases(harness string) []safetyTierCase {
	return []safetyTierCase{
		{"git checkout -b feature/x origin/main", harness, core.Allow},
		{"git switch -c feature/y", harness, core.Allow},
		{"git restore --staged x.go", harness, core.Allow},
		{"git reset", harness, core.Allow},
		{"git reset -q -- x.go", harness, core.Allow},
		{"git reset HEAD x.go", harness, core.Allow},
		{`T=/tmp && git -C "$T" stash`, harness, core.Allow},
		{`T=$(mktemp -d) && git -C "$T" checkout -- x`, harness, core.Allow},
		{`cd "$(mktemp -d -t x)" && git stash`, harness, core.Allow},
		{"git checkout -B main", harness, core.Block},
		{"git checkout -b x -f", harness, core.Block},
		{"git restore -SW x.go", harness, core.Block},
		{"git reset x.go", harness, core.Block},
		{`cd "$(mktemp -d -p .)" && git reset --hard`, harness, core.Block},
		{`T=$(mktemp -d scratch.XXXXXX) && cd "$T" && git stash`, harness, core.Block},
		{"T=" + harness + ` && git -C "$T" stash`, "", core.Block},
	}
}

// A ' inside "…" is literal, so a $(…) or backtick after it still runs and must reach the shared-config git check.
func TestHarnessGitBlockedApostropheInDoubleQuotes(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, cmd := range []string{`git commit -m "it's $(git stash)"`, "git commit -m \"it's `git reset --hard`\""} {
		if harnessGitVerb(cmd, cfg) == "" {
			t.Errorf("harnessGitVerb(%q) = false, want true", cmd)
		}
	}
}

// cd options and prefix wrappers must not hide a banned git verb in the shared config tree.
func TestHarnessGitBlockedOptionsAndWrappers(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	blocked := []string{
		"cd -P " + cfg + " && git stash", "cd -L " + cfg + " && git stash", "cd -- " + cfg + " && git reset --hard",
		"env bash -c 'git stash'", `timeout 5 sh -c "git -C ` + cfg + ` reset --hard"`, "sudo -u root bash -c 'git stash'",
		"nohup bash -c 'git stash'", `env FOO=1 eval "git stash"`, "command bash -c 'git stash'",
		"exec sh -c 'git stash'", "time bash -c 'git stash'", "nice -n 5 bash -c 'git stash'",
	}
	for _, cmd := range blocked {
		if harnessGitVerb(cmd, cfg) == "" {
			t.Errorf("harnessGitVerb(%q) = false, want true", cmd)
		}
		if strings.HasPrefix(cmd, "cd ") && harnessGitVerb(cmd, t.TempDir()) == "" {
			t.Errorf("harnessGitVerb(%q) from another cwd = false, want true", cmd)
		}
	}
}

// TestHarnessGitNotBlockedOptionsAndWrappers: incomplete wrappers and read-only verbs stay allowed.
func TestHarnessGitNotBlockedOptionsAndWrappers(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, cmd := range []string{"cd -P", "env", "timeout", "sudo -u", "cd -P " + cfg + " && git stash list", "env bash -c 'git status'"} {
		if harnessGitVerb(cmd, cfg) != "" {
			t.Errorf("harnessGitVerb(%q) = true, want false", cmd)
		}
	}
}

// TestSafetyHarnessGitWrapperForms: value options, env -S and cd/chdir forms still reach the banned-verb check.
func TestSafetyHarnessGitWrapperForms(t *testing.T) {
	harness := hosttest.TierHarness(t)
	cases := []struct{ cmd, cwd string }{
		{"sudo --user root bash -c 'git stash'", harness},
		{"timeout -s KILL 5 bash -c 'git stash'", harness},
		{"timeout -k 1 5 bash -c 'git stash'", harness},
		{"exec -a x bash -c 'git stash'", harness},
		{`env -S "bash -c 'git stash'"`, harness},
		{"if cd " + harness + "; then git stash; fi", "/tmp"},
		{"CDPATH=x cd " + harness + " && git stash", "/tmp"},
		{"env -C " + harness + " git stash", "/tmp"},
		{"sudo -D " + harness + " git stash", "/tmp"},
		{"env --chdir=" + harness + " git stash", "/tmp"},
	}
	for _, c := range cases {
		if harnessGitVerb(c.cmd, c.cwd) == "" {
			t.Errorf("harnessGitVerb(%q, %q) = false, want true", c.cmd, c.cwd)
		}
	}
	if harnessGitVerb("env -C /tmp git stash && git status", harness) != "" {
		t.Errorf("env -C /tmp must not leak into the config tree, nor later segments block")
	}
}

// TestHarnessGitBlocked pins harnessGitVerb: config-tree bans, the worktree exception, targets, cd and nesting.
func TestHarnessGitBlocked(t *testing.T) {
	cfg, outside := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	lane := filepath.Join(outside, "lane") // a linked worktree of the harness repository, outside the config dir
	if err := os.MkdirAll(lane, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lane, ".git"), []byte("gitdir: "+cfg+"/.git/worktrees/lane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range harnessGitCases(cfg, outside) {
		t.Run(tt.name, func(t *testing.T) {
			if got := harnessGitVerb(tt.cmd, tt.cwd) != ""; got != tt.want {
				t.Errorf("harnessGitVerb(%q, %q) = %v, want %v", tt.cmd, tt.cwd, got, tt.want)
			}
		})
	}
}

type harnessGitCase struct {
	name string
	cmd  string
	cwd  string
	want bool
}

// harnessGitCases lists TestHarnessGitBlocked's commands; want is whether harnessGitVerb flags them.
func harnessGitCases(cfg, outside string) []harnessGitCase {
	worktree, lane := filepath.Join(cfg, ".claude", "worktrees", "repo", "slug"), filepath.Join(outside, "lane")
	return append([]harnessGitCase{
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
		{"GIT_DIR and GIT_WORK_TREE into tree", "GIT_DIR=" + cfg + "/.git GIT_WORK_TREE=" + cfg + " git stash", outside, true},
		{"env GIT_DIR into tree", "env GIT_DIR=" + cfg + "/.git git stash", outside, true},
		{"export GIT_DIR into tree", "export GIT_DIR=" + cfg + "/.git; git stash", outside, true},
		{"GIT_DIR outside tree, cwd outside", "GIT_DIR=" + outside + "/.git git stash", outside, false},
		{"GIT_DIR outside, cwd is the tree", "GIT_DIR=" + outside + "/.git git stash", cfg, true},
		{"--git-dir outside, cwd is the tree", "git --git-dir=" + outside + "/.git stash", cfg, true},
		{"git dir in tree, work tree outside", "git --git-dir=" + cfg + "/.git --work-tree=" + outside + " stash", outside, true},
		{"GIT_DIR outside, GIT_WORK_TREE the tree", "GIT_DIR=" + outside + "/.git GIT_WORK_TREE=" + cfg + " git clean -fd", outside, true},
		{"--git-dir and --work-tree outside", "git --git-dir=" + outside + "/.git --work-tree=" + outside + " reset --hard", outside, false},
		{"GIT_DIR and GIT_WORK_TREE outside, cwd is the tree", "GIT_DIR=" + outside + "/.git GIT_WORK_TREE=" + outside + " git checkout main", cfg, false},
		{"linked worktree's own git dir", "git --git-dir=" + cfg + "/.git/worktrees/x --work-tree=" + outside + " checkout -- f", cfg, false},
		{"GIT_DIR into tree through sh -c", "GIT_DIR=" + cfg + "/.git sh -c 'git stash'", outside, true},
		{"GIT_DIR outside, unguarded verb", "GIT_DIR=" + outside + "/.git git status", cfg, false},
		{"stash in a harness lane", "git stash", lane, true},
		{"stash push in a harness lane subdir", "git stash push -m x", filepath.Join(lane, "internal"), true},
		{"-C stash into a harness lane", "git -C " + lane + " stash pop", outside, true},
		{"stash list in a harness lane allowed", "git stash list", lane, false},
		{"stash show in a harness lane allowed", "git stash show", lane, false},
		{"reset in a harness lane allowed", "git reset --hard HEAD", lane, false},
		{"checkout in a harness lane allowed", "git checkout main", lane, false},
		{"clean in a harness lane allowed", "git clean -fd", lane, false},
	}, harnessGitNestedCases(cfg, outside)...)
}

// harnessGitNestedCases are the substitution, nested-shell, cd-tracking and hidden-verb rows.
func harnessGitNestedCases(cfg, outside string) []harnessGitCase {
	return []harnessGitCase{
		{"command substitution into tree", "echo $(cd " + cfg + " && git reset --hard)", outside, true},
		{"nested bash -c into tree", `bash -c "git reset --hard"`, cfg, true},
		{"nested bash -c outside tree", `bash -c "git reset --hard"`, outside, false},
		{"eval into tree", "eval git reset --hard", cfg, true},
		{"non-git command", "ls -la", cfg, false},
		{"nesting too deep to check", "eval eval eval eval eval git restore .", cfg, true},
		{"deep nesting outside tree", "eval eval eval eval eval git restore .", outside, false},
		{"case pattern list is not a pipe into a shell", "for i in 1 2; do case \"$(basename x)\" in sh|bash|zsh) p=1;; *) break;; esac; done", cfg, false},
		{"too deep, no guarded word", "eval eval eval eval eval ls -la", cfg, false},
		{"too deep, word as a file name", "eval eval eval eval eval cat reset.txt", cfg, true},
		{"expanded verb is not nesting", "v=x; git $v .", cfg, true},
		{"3-deep nesting of git status", "eval eval eval git status", cfg, false},
		{"env cd does not move the shell", "env cd " + outside + "; git restore .", cfg, true},
		{"nohup cd does not move the shell", "nohup cd " + outside + "; git stash", cfg, true},
		{"command cd moves the shell", "command cd " + outside + "; git stash", cfg, false},
		{"bash -c string then a positional", "bash -c 'git stash' -n", cfg, true},
		{"ssh command runs remotely", "ssh devhost git reset --hard", cfg, false},
		{"ssh heredoc runs remotely", "ssh devhost 'bash -s' > log 2>&1 <<'EOF'\ngit checkout -q HEAD\nEOF", cfg, false},
		{"local git after ssh heredoc", "ssh devhost <<'EOF'\nuptime\nEOF\ngit reset --hard", cfg, true},
		{"bash heredoc stays local", "bash -s <<'EOF'\ngit reset --hard\nEOF", cfg, true},
		{"ssh then a local bash heredoc", "ssh devhost true; bash <<'EOF'\ngit checkout .\nEOF", cfg, true},
		{"subshell cd does not leak", "(cd " + outside + "); git restore .", cfg, true},
		{"cd to a missing dir keeps cwd", "cd " + outside + "/missing_zz 2>/dev/null; git restore .", cfg, true},
		{"|| after a cd keeps cwd", "cd " + outside + " || git restore .", cfg, true},
		{"pushd then popd", "pushd " + outside + "; popd; git restore .", cfg, true},
		{"setsid sh -c", "setsid sh -c 'git restore .'", cfg, true},
		{"watch string", "watch 'git restore .'", cfg, true},
		{"echo piped to bash", "echo 'git restore .' | bash", cfg, true},
		{"here-string to bash", "bash <<< 'git restore .'", cfg, true},
		{"alias hides the verb", "git -c alias.zz=restore zz .", cfg, true},
		{"--super-prefix takes a value", "git --super-prefix x/ restore .", cfg, true},
		{"--config-env takes a value", "git --config-env core.x=HOME restore .", cfg, true},
		{"expanded verb", "git $(echo restore) .", cfg, true},
		{"ANSI-C verb", "git $'restore' .", cfg, true},
		{"env -C attached", "env -C" + cfg + " git restore .", outside, true},
		{"echo text is inert", "echo 'git restore .'", cfg, false},
		{"real cd away is allowed", "cd " + outside + " && git restore .", cfg, false},
		{"ssh heredoc piped to a local shell", "ssh devhost <<'EOF' | bash\ngit reset --hard\nEOF", cfg, true},
	}
}

// Hard rule 4 never blocks on text a heredoc only writes to a file; a body the same command runs still blocks.
func TestHarnessGitIgnoresDataHeredocBodies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	cases := []struct {
		name, cmd string
		want      core.Decision
	}{
		{"brief written to a file", "mkdir -p /tmp/a && cat > /tmp/a/task.md <<'EOF'\nNever git stash|checkout|reset|restore|clean.\nEOF", core.Allow},
		{"tee data", "tee notes.md <<'EOF'\ngit reset --hard\nEOF", core.Allow},
		{"written then run", "cat > f.sh <<'EOF'\ngit reset --hard\nEOF\nbash f.sh", core.Block},
		{"run on the start line", "cat > f.sh <<'EOF' && sh f.sh\ngit stash\nEOF", core.Block},
		{"chmod then run", "cat > f <<'EOF'\ngit checkout .\nEOF\nchmod +x f && ./f", core.Block},
		{"unquoted delimiter, no substitution", "cat > f.md <<EOF\ngit stash\nEOF", core.Allow},
		{"executed heredoc", "bash <<'EOF'\ngit reset --hard\nEOF", core.Block},
		{"git after the heredoc", "cat > f.md <<'EOF'\nhi\nEOF\ngit reset --hard", core.Block},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": home, "tool_input": map[string]string{"command": c.cmd}})
			if got, stderr := safetyScanDecision(t, bytes.NewReader(payload)); got != c.want {
				t.Errorf("got %d want %d (%s)", got, c.want, stderr)
			}
		})
	}
}
