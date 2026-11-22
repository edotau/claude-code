package worktree

import (
	"bufio"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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
var inUsePoll = 100 * time.Millisecond

// parentOf returns pid's parent pid; ancestry walks it for InUseBy's exclusion and PeerAgents.
var parentOf func(pid int) (int, error)

// ProcessScan exposes callback pointers so test helpers can replace process scans without importing this package.
var ProcessScan = struct {
	In     *func(string) ([]Proc, error)
	Up     *func(int) (int, error)
	Procs  *func() ([]procCwd, error)
	Agents *func() ([]procCwd, error)
}{&processesIn, &parentOf, &scanProcs, &scanAgentProcs}

// InUseBy returns a refusal reason when the caller's cwd or another live non-ancestor process is in dest; "" = clear.
func InUseBy(dest string) (string, error) { return inUseBy(dest, nil) }

// inUseBy is InUseBy with an optional lister; nil means liveLister.
func inUseBy(dest string, in func(string) ([]Proc, error)) (string, error) {
	real := paths.CanonDeep(dest) // a vanished dest still resolves its parents (/var -> /private/var)
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
		// no detector wired for this OS
		return "", nil
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
			return fmt.Sprintf("in use by pid %d (%s) with its cwd inside %s; stop it or wait for it to exit, then retry",
				busy.PID, busy.Comm, real), nil
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
		if scanMemoOn.Load() {
			return memoLooker
		}
		return scanIn
	}
	return nil
}

// scanMemo shares one process scan across a verb's checks; a dir found busy is always re-asked live.
var scanMemo = paths.NewMemo[struct{}, []procCwd]("worktree.process-scan")
var scanMemoOn atomic.Bool

// scanMemoTTL bounds how stale a shared scan may be, so a long gate between two checks forces a fresh one.
const scanMemoTTL = 5 * time.Second

// BeginScanMemo starts sharing process scans until the returned func is called; one verb at a time.
func BeginScanMemo() (end func()) {
	scanMemo.Drop()
	scanMemoOn.Store(true)
	return func() {
		scanMemoOn.Store(false)
		scanMemo.Drop()
	}
}

// memoLooker answers "nothing inside dir" from the shared scan and re-asks live when something is there.
func memoLooker(dir string) ([]Proc, error) {
	all, err := scanMemo.Do(struct{}{}, func(_ []procCwd, err error) time.Duration {
		if err != nil {
			return -1
		}
		return scanMemoTTL
	}, scanProcs)
	if err != nil {
		return nil, err
	}
	if len(filterProcs(all, dir)) == 0 {
		return nil, nil
	}
	return scanIn(dir)
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

// scanAgentProcs is scanProcs limited to agent-named processes; nil means PeerAgents uses scanProcs.
var scanAgentProcs func() ([]procCwd, error)

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
		procs = append(procs, Proc{PID: p.PID, Comm: agentLabel(p.PID, p.Comm)})
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
	scan := scanProcs
	if scanAgentProcs != nil {
		scan = scanAgentProcs
	}
	if scan == nil {
		return nil, nil
	}
	all, err := scan()
	if err != nil && scanProcs != nil {
		// the agent-only scan is an optimisation: when it cannot list pids, ask the full scan instead
		all, err = scanProcs()
	}
	if err != nil {
		return nil, err
	}
	skip := pidsWithin(all, append([]string{filepath.Join(checkout, ".git")}, linked...))
	mine := ancestry(os.Getpid())
	own := agentAncestors(mine)
	jobs := sync.OnceValue(func() map[int]bool { return ownJobs(mine) })
	var out []Proc
	for _, p := range filterProcs(all, checkout) {
		if !agentComms[p.Comm] || skip[p.PID] || mine[p.PID] || descendsFrom(p.PID, own) || isDaemon(p.PID) {
			continue
		}
		// a background dispatch detaches (Setsid) and reparents, so ancestry alone misses this session's own jobs
		if descendsFrom(p.PID, jobs()) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ownJobs is the pids of live dispatch jobs owned by a pid in mine (never init); a seam so tests skip host records.
var ownJobs = func(mine map[int]bool) map[int]bool {
	jobs := map[int]bool{}
	for _, r := range agent.RecentRecords(agent.AllRecords) {
		if r.PID <= 0 || r.Owner <= 1 || !mine[r.Owner] || !r.Alive() || r.PIDStart == "" {
			continue
		}
		// a reused pid is not the job: its start time differs from the one recorded at spawn
		if start, err := proc.StartTime(r.PID); err == nil && start == r.PIDStart {
			jobs[r.PID] = true
		}
	}
	return jobs
}

// agentAncestors returns the caller's ancestors that are themselves agents by name.
func agentAncestors(mine map[int]bool) map[int]bool {
	own := map[int]bool{}
	if commName == nil {
		return own
	}
	for pid := range mine {
		if agentComms[agentLabel(pid, commName(pid))] {
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

// helperSubcmds are claude subcommands that manage config or installs and never edit a checkout.
var helperSubcmds = map[string]bool{"update": true, "upgrade": true, "doctor": true, "auth": true, "setup-token": true}

// isDaemon reports a non-session helper: agy's IDE `--hub`, Chrome's native host, or a claude admin subcommand.
func isDaemon(pid int) bool {
	argv, err := argvOf(pid)
	return err == nil && isHelperArgv(argv)
}

// isClaudeBinary matches claude by name or by its version-named binary (2.1.325); other agents' words are prompts.
func isClaudeBinary(argv0 string) bool {
	base := strings.TrimSuffix(filepath.Base(argv0), ".exe")
	return base == "claude" || base != "" && base[0] >= '0' && base[0] <= '9'
}

// isHelperArgv classifies argv; print mode (-p) and `mcp serve` still edit, so they stay peers.
func isHelperArgv(argv []string) bool {
	if len(argv) > 1 && helperSubcmds[argv[1]] && isClaudeBinary(argv[0]) {
		return true
	}
	return slices.ContainsFunc(argv, func(a string) bool {
		return strings.HasPrefix(a, "--hub") || a == "--chrome-native-host"
	})
}

// agentLabel is comm, or the agent a version-named binary belongs to (Claude Code's kernel name is "2.1.325").
func agentLabel(pid int, comm string) string {
	return cmp.Or(proc.AgentName(comm, func() string { return exeOf(pid) }, agentComms), comm)
}

// exeOf reads a pid's executable path; a seam so tests never read the host's process table.
var exeOf = proc.Exe

// argvOf reads a pid's argv; a seam so tests never shell out.
var argvOf = proc.Argv

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
