package worktree

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
)

// Proc identifies a live process with a cwd inside a worktree that Clean is about to remove.
type Proc struct {
	PID  int
	Comm string
}

// processesIn is a test override of the per-dir process lister; nil means scanIn over scanProcs.
var processesIn func(dir string) ([]Proc, error)

// inUseSettle is how long a busy worktree is re-checked before refusing: git's auto gc exits on its own.
var inUseSettle = time.Second

// inUsePoll is the pause between settle re-checks of a busy worktree.
const inUsePoll = 100 * time.Millisecond

// parentOf returns pid's parent pid; ancestry walks it for InUseBy's exclusion and PeerAgents.
var parentOf func(pid int) (int, error)

// platformScan holds the OS implementations StubProcessScanForTest replaced, so a real-process test can restore them.
var platformScan struct {
	up    func(int) (int, error)
	procs func() ([]procCwd, error)
}

// StubProcessScanForTest makes the process scan (lsof, ps) report nothing for a test binary; call from TestMain.
func StubProcessScanForTest() {
	platformScan.up, platformScan.procs = parentOf, scanProcs
	processesIn = func(string) ([]Proc, error) { return nil, nil }
	parentOf = func(int) (int, error) { return 0, nil }
	scanProcs = func() ([]procCwd, error) { return nil, nil }
}

// InUseBy returns a refusal reason when the caller's cwd or another live non-ancestor process is in dest; "" = clear.
func InUseBy(dest string) (string, error) { return inUseBy(dest, nil) }

// inUseBy is InUseBy with an optional lister; nil means liveLister.
func inUseBy(dest string, in func(string) ([]Proc, error)) (string, error) {
	real, err := filepath.EvalSymlinks(dest)
	if err != nil {
		real = dest // not resolvable yet; compare against the raw path
	}
	inside, err := cwdInside(real)
	if err != nil {
		return "", err
	}
	if inside {
		return "current directory is inside it; cd out first", nil
	}
	if in == nil {
		in = liveLister()
	}
	if in == nil {
		return "", nil // no detector wired for this OS
	}
	return settleForeign(in, real)
}

// cwdInside reports whether the calling process's own cwd resolves inside real.
func cwdInside(real string) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, fmt.Errorf("resolve current directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return false, fmt.Errorf("resolve current directory: %w", err)
	}
	return paths.Within(resolved, real), nil
}

// settleForeign re-checks a busy dir for inUseSettle before returning the refusal for the first foreign process.
func settleForeign(in func(string) ([]Proc, error), real string) (string, error) {
	excluded := ancestry(os.Getpid())
	deadline := time.Now().Add(inUseSettle)
	for {
		busy, err := foreignProc(in, real, excluded)
		if err != nil || busy == nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("in use by pid %d (%s)", busy.PID, busy.Comm), nil
		}
		time.Sleep(inUsePoll)
	}
}

// foreignProc returns the first live process inside dir that is not the caller or one of its ancestors.
func foreignProc(in func(string) ([]Proc, error), dir string, excluded map[int]bool) (*Proc, error) {
	procs, err := in(dir)
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

// liveLister is the per-dir process lister: the processesIn override if set, else one live scan; nil = no detector.
func liveLister() func(string) ([]Proc, error) {
	if processesIn != nil {
		return processesIn
	}
	if scanProcs != nil {
		return scanIn
	}
	return nil
}

// firstLook scans once and answers "nothing inside" from it; a busy dir is re-asked live so the settle loop sees exits.
func firstLook() func(string) ([]Proc, error) {
	if processesIn != nil || scanProcs == nil {
		return liveLister()
	}
	all, err := scanProcs()
	return func(dir string) ([]Proc, error) {
		if err != nil {
			return nil, err
		}
		if len(filterProcs(all, dir)) == 0 {
			return nil, nil
		}
		return scanIn(dir)
	}
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
var agentComms = func() map[string]bool {
	set := map[string]bool{}
	for _, a := range agentid.All() {
		set[agentid.Binary(a)] = true
	}
	return set
}()

// procCwd is one live process and its resolved cwd; Comm is empty when the scan did not read it.
type procCwd struct {
	PID  int
	Cwd  string
	Comm string
}

// scanProcs lists every inspectable process with its cwd in one pass; each platform's init() assigns it.
var scanProcs func() ([]procCwd, error)

// filterProcs keeps the scanned processes whose cwd is dir or under it, reading a missing comm only for those.
func filterProcs(all []procCwd, dir string) []Proc {
	var procs []Proc
	for _, p := range all {
		if !paths.Within(p.Cwd, dir) {
			continue
		}
		if p.Comm == "" && commName != nil {
			p.Comm = commName(p.PID)
		}
		procs = append(procs, Proc{PID: p.PID, Comm: p.Comm})
	}
	return procs
}

// scanIn is processesIn on a platform with scanProcs: one scan, filtered to dir.
func scanIn(dir string) ([]Proc, error) {
	all, err := scanProcs()
	if err != nil {
		return nil, err
	}
	return filterProcs(all, dir), nil
}

// PeerAgents lists agent processes in checkout (not .git, linked worktrees, the caller's ancestry or its descendants).
func PeerAgents(checkout string, linked []string) ([]Proc, error) {
	if scanProcs == nil {
		return nil, nil
	}
	all, err := scanProcs()
	if err != nil {
		return nil, err
	}
	skip := pidsWithin(all, append([]string{filepath.Join(checkout, ".git")}, linked...))
	mine := ancestry(os.Getpid())
	own := agentAncestors(mine)
	var out []Proc
	for _, p := range filterProcs(all, checkout) {
		if !agentComms[p.Comm] || skip[p.PID] || mine[p.PID] || descendsFrom(p.PID, own) || isDaemon(p.PID) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// agentAncestors returns the caller's ancestors that are themselves agents by name.
func agentAncestors(mine map[int]bool) map[int]bool {
	own := map[int]bool{}
	if commName == nil {
		return own
	}
	for pid := range mine {
		if agentComms[commName(pid)] {
			own[pid] = true
		}
	}
	return own
}

// pidsWithin returns the pids of all whose cwd is inside any of dirs.
func pidsWithin(all []procCwd, dirs []string) map[int]bool {
	pids := map[int]bool{}
	for _, p := range all {
		if slices.ContainsFunc(dirs, func(d string) bool { return paths.Within(p.Cwd, d) }) {
			pids[p.PID] = true
		}
	}
	return pids
}

// isDaemon reports a background agent server rather than a working session (agy's `--hub` from the IDE extension).
func isDaemon(pid int) bool {
	out, err := procArgs(pid)
	return err == nil && slices.ContainsFunc(strings.Fields(out), func(a string) bool { return strings.HasPrefix(a, "--hub") })
}

// procArgs reads a pid's command line (linux's init swaps in /proc to avoid forking); a seam so tests never shell out.
var procArgs = func(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return string(out), err
}

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

// parseLsofCwd reads `lsof -Fpcn` output: a p(id) line opens a process, c(omm) names it, each n(ame) is its cwd.
func parseLsofCwd(out string) ([]procCwd, error) {
	var procs []procCwd
	var pid int
	var comm string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			comm = ""
		case 'c':
			comm = line[1:]
		case 'n':
			procs = append(procs, procCwd{PID: pid, Cwd: line[1:], Comm: comm})
		}
	}
	return procs, scanner.Err()
}
