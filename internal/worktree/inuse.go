package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Proc identifies a live process with a cwd inside a worktree that Clean is about to remove.
type Proc struct {
	PID  int
	Comm string
}

// processesIn finds live processes whose cwd is dir or under it; each platform's init() assigns the
// real implementation (linux: /proc/*/cwd, darwin: lsof), seamed here so tests can stub it.
var processesIn func(dir string) ([]Proc, error)

// inUseSettle is how long a busy worktree is re-checked before refusing: git's own background maintenance
// (auto gc after a commit) briefly runs inside it and exits on its own.
var inUseSettle = time.Second

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
	excluded := ancestry(os.Getpid())
	deadline := time.Now().Add(inUseSettle)
	for {
		busy, err := foreignProc(real, excluded)
		if err != nil || busy == nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("in use by pid %d (%s)", busy.PID, busy.Comm), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// foreignProc returns the first live process inside dir that is not the caller or one of its ancestors.
func foreignProc(dir string, excluded map[int]bool) (*Proc, error) {
	procs, err := processesIn(dir)
	if err != nil {
		return nil, err
	}
	for _, p := range procs {
		if !excluded[p.PID] {
			return &p, nil
		}
	}
	return nil, nil
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

// agentComms are the process names that count as a live agent session in PeerAgents.
var agentComms = map[string]bool{"claude": true, "codex": true, "copilot": true, "opencode": true, "agy": true}

// PeerAgents lists agent processes with a cwd inside checkout (not .git, not a linked worktree), excluding the
// caller's ancestry and everything descended from an agent in it. Linked worktrees are passed in as linked.
func PeerAgents(checkout string, linked []string) ([]Proc, error) {
	if processesIn == nil {
		return nil, nil
	}
	procs, err := processesIn(checkout)
	if err != nil {
		return nil, err
	}
	skip := map[int]bool{}
	for _, dir := range append([]string{filepath.Join(checkout, ".git")}, linked...) {
		inner, _ := processesIn(dir)
		for _, p := range inner {
			skip[p.PID] = true
		}
	}
	mine := ancestry(os.Getpid())
	own := map[int]bool{}
	for pid := range mine {
		if commKnown(pid) {
			own[pid] = true
		}
	}
	var out []Proc
	for _, p := range procs {
		if !agentComms[p.Comm] || skip[p.PID] || mine[p.PID] || descendsFrom(p.PID, own) || isDaemon(p.PID) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// isDaemon reports a background agent server rather than a working session (agy's `--hub` from the IDE extension).
func isDaemon(pid int) bool {
	out, err := procArgs(pid)
	return err == nil && slices.ContainsFunc(strings.Fields(out), func(a string) bool { return strings.HasPrefix(a, "--hub") })
}

// procArgs reads a pid's command line; a seam so tests never shell out.
var procArgs = func(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return string(out), err
}

// commKnown reports whether pid is itself an agent process by name.
func commKnown(pid int) bool { return commName != nil && agentComms[commName(pid)] }

// commName reads a pid's process name; each platform's init() assigns it where available.
var commName func(pid int) string

// descendsFrom reports whether pid's parent chain passes through any pid in roots.
func descendsFrom(pid int, roots map[int]bool) bool {
	for anc := range ancestry(pid) {
		if anc != pid && roots[anc] {
			return true
		}
	}
	return false
}
