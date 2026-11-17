// Git exec seam: sanitized environment, caller-supplied timeout via ctx, output trimmed only on the right.

package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GitEnv is the process environment minus every GIT_* variable, which would otherwise override a -C flag.
func GitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// RunGit executes git with GitEnv; dir becomes -C when non-empty (pass "" if args carries its own).
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, GitEnv(), dir, args...)
}

// RunGitInherit runs git with the caller's environment, for commands inside a git hook that need its GIT_* state.
//
// Gotcha: internal/configure and internal/memory must keep RunGit so an inherited GIT_DIR cannot retarget them.
func RunGitInherit(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, os.Environ(), dir, args...)
}

// RunGitRaw is RunGit with stdout returned byte-for-byte, trailing newlines kept (file contents).
func RunGitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return runRaw(ctx, GitEnv(), dir, args...)
}

// RunGitInheritRaw is RunGitInherit with stdout returned byte-for-byte, trailing newlines kept (file contents).
func RunGitInheritRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return runRaw(ctx, os.Environ(), dir, args...)
}

func run(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	out, err := runRaw(ctx, env, dir, args...)
	return strings.TrimRight(out, "\n"), err
}

func runRaw(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = env
	cmd.WaitDelay = ExecWaitDelay // a descendant holding stdout open must not outlive ctx's deadline
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// The full argv, not just the subcommand: a wrong --git-dir or -C IS the usual failure here,
		// and the message is the only place it becomes visible.
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(full, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// StatusEntry is one `git status --porcelain=v1 -z` record; From is a rename or copy's source path.
type StatusEntry struct{ Code, Path, From string }

// StatusEntries parses `git status --porcelain=v1 -z` output; R or C in either code column marks a rename or copy.
func StatusEntries(out string) []StatusEntry {
	var entries []StatusEntry
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		e := StatusEntry{Code: f[:2], Path: f[3:]}
		if strings.ContainsAny(e.Code, "RC") && i+1 < len(fields) {
			i++
			e.From = fields[i]
		}
		entries = append(entries, e)
	}
	return entries
}
