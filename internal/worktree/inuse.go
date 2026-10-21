package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Proc identifies a live process with a cwd inside a worktree that Clean is about to remove.
type Proc struct {
	PID  int
	Comm string
}

// processesIn finds live processes whose cwd is dir or under it; each platform's init() assigns the
// real implementation (linux: /proc/*/cwd, darwin: lsof), seamed here so tests can stub it.
var processesIn func(dir string) ([]Proc, error)

// parentOf returns pid's parent pid, used only to exclude the calling process's own ancestry below.
var parentOf func(pid int) (int, error)

// InUseBy reports a refusal reason if dest is unsafe to remove right now: the calling process's own
// cwd is inside it ("cd out first"), or another live process (not the caller or an ancestor) has its
// cwd inside it ("in use by pid N (<comm>)"). "" means clear. Resolves symlinks on both sides first.
func InUseBy(dest string) (string, error) {
	real, err := filepath.EvalSymlinks(dest)
	if err != nil {
		real = dest // not resolvable yet; compare against the raw path
	}
	if cwd, err := os.Getwd(); err == nil {
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil && within(resolved, real) {
			return "current directory is inside it; cd out first", nil
		}
	}
	if processesIn == nil {
		return "", nil // no detector wired for this OS
	}
	procs, err := processesIn(real)
	if err != nil {
		return "", err
	}
	excluded := ancestry(os.Getpid())
	for _, p := range procs {
		if excluded[p.PID] {
			continue
		}
		return fmt.Sprintf("in use by pid %d (%s)", p.PID, p.Comm), nil
	}
	return "", nil
}

// within reports whether path equals base or sits under it, comparing with a trailing separator so
// /a/foo does not match /a/foobar.
func within(path, base string) bool {
	path = strings.TrimRight(path, string(filepath.Separator))
	base = strings.TrimRight(base, string(filepath.Separator))
	return path == base || strings.HasPrefix(path, base+string(filepath.Separator))
}

// ancestry walks pid's parent chain so InUseBy can exclude the caller's own process tree.
func ancestry(pid int) map[int]bool {
	seen := map[int]bool{}
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		if parentOf == nil {
			break
		}
		ppid, err := parentOf(pid)
		if err != nil {
			break
		}
		pid = ppid
	}
	return seen
}
