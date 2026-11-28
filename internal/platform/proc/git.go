// Git exec seam: sanitized environment, caller-supplied timeout via ctx, output trimmed only on the right.

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/textutil"
)

// GitEnv is the process environment minus every GIT_* variable, which would otherwise override a -C flag.
func GitEnv() []string { return gitEnv(false) }

func runGitEnv() []string { return gitEnv(os.Getenv("HOSTTEST_ACTIVE") != "") }

func gitEnv(keepTest bool) []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !EnvHasPrefix(kv, "GIT_") || keepTest && keepTestGitVar(kv) {
			out = append(out, kv)
		}
	}
	return out
}

func keepTestGitVar(kv string) bool {
	return slices.ContainsFunc([]string{"GIT_CONFIG_", "GIT_TERMINAL_PROMPT=", "GIT_OPTIONAL_LOCKS=", "GIT_AUTHOR_", "GIT_COMMITTER_"},
		func(p string) bool { return EnvHasPrefix(kv, p) })
}

// RunGit executes git with GitEnv; dir becomes -C when non-empty (pass "" if args carries its own).
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, runGitEnv(), dir, args...)
}

// Git runs git in dir with GIT_* scrubbed: ok=false when git itself refused; err for a missing binary or a timeout.
func Git(ctx context.Context, dir string, args ...string) (lines []string, ok bool, err error) {
	out, err := RunGit(ctx, dir, args...)
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return nil, false, ctx.Err()
	case errors.As(err, &exit):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return strings.Split(strings.TrimSpace(out), "\n"), true, nil
}

// GitTimeout is RunGit bounded by d.
func GitTimeout(d time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return RunGit(ctx, dir, args...)
}

// GitInheritTimeout is RunGitInherit under a 30s deadline, trailing newlines trimmed.
func GitInheritTimeout(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return RunGitInherit(ctx, dir, args...)
}

// GitRawTimeout is RunGitRaw under a 30s deadline, stdout kept byte-for-byte.
func GitRawTimeout(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return RunGitRaw(ctx, dir, args...)
}

// GitNUL runs git (args carry -z) and returns its NUL-separated fields, empty ones dropped.
func GitNUL(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := RunGitRaw(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	return textutil.SplitNUL(out), nil
}

// RunGitInherit runs git with the caller's GIT_* env for git-hook commands; configure and memory must keep RunGit.
func RunGitInherit(ctx context.Context, dir string, args ...string) (string, error) {
	return run(ctx, os.Environ(), dir, args...)
}

// RunGitRaw is RunGit with stdout returned byte-for-byte, trailing newlines kept (file contents).
func RunGitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return runRaw(ctx, runGitEnv(), dir, args...)
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
	if n := strings.Count(out, "\x00"); n > 0 {
		entries = make([]StatusEntry, 0, n) // one allocation; a clean tree's "" stays nil
	}
	for out != "" {
		var f string
		f, out, _ = strings.Cut(out, "\x00")
		if len(f) < 4 {
			continue
		}
		e := StatusEntry{Code: f[:2], Path: f[3:]}
		if (f[0] == 'R' || f[0] == 'C' || f[1] == 'R' || f[1] == 'C') && out != "" {
			e.From, out, _ = strings.Cut(out, "\x00")
		}
		entries = append(entries, e)
	}
	return entries
}
