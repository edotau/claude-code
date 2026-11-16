package worktree

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// stubProcesses swaps processesIn/parentOf and restores the platform originals on cleanup.
func stubProcesses(t *testing.T, in func(dir string) ([]Proc, error)) {
	t.Helper()
	origIn, origParent, origSettle, origAgents := processesIn, parentOf, inUseSettle, scanAgentProcs
	processesIn, scanAgentProcs = in, nil
	parentOf = func(pid int) (int, error) { return 0, nil }
	// stubbed processes never exit; refuse on the first look
	inUseSettle = 0
	t.Cleanup(func() {
		processesIn, parentOf, inUseSettle, scanAgentProcs = origIn, origParent, origSettle, origAgents
	})
}

// A process that exits within the settle window (git's background auto-gc) does not block removal.
func TestInUseByWaitsOutTransientProcess(t *testing.T) {
	dest := t.TempDir()
	calls := 0
	stubProcesses(t, func(dir string) ([]Proc, error) {
		calls++
		if calls <= 2 {
			return []Proc{{PID: 999999, Comm: "git"}}, nil
		}
		return nil, nil
	})
	inUseSettle = time.Second
	reason, err := InUseBy(dest)
	if err != nil || reason != "" {
		t.Fatalf("InUseBy = %q, %v; want clear once the transient process exits", reason, err)
	}
	if calls < 3 {
		t.Errorf("checked %d times, want a re-check after the first busy look", calls)
	}
}

func TestInUseByRefusesWhenStubbedProcessIsInside(t *testing.T) {
	dest := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) {
		return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil
	})
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal reason for a stubbed process inside dest")
	}
	if !strings.Contains(reason, "999999") || !strings.Contains(reason, "fakeproc") {
		t.Errorf("reason = %q, want it to name the pid and comm", reason)
	}
}

func TestInUseByExcludesCallerAndAncestors(t *testing.T) {
	dest := t.TempDir()
	caller := os.Getpid()
	stubProcesses(t, func(dir string) ([]Proc, error) {
		return []Proc{{PID: caller, Comm: "self"}}, nil
	})
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want the caller's own pid excluded", reason)
	}
}

func TestInUseBySymlinkedPathMatched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// darwin's TempDir is itself under the /var -> /private/var symlink
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	stubProcesses(t, func(dir string) ([]Proc, error) {
		// production processesIn implementations report the resolved cwd; dir passed in is already resolved.
		if paths.Within(resolved, dir) {
			return []Proc{{PID: 999999, Comm: "throughlink"}}, nil
		}
		return nil, nil
	})
	// InUseBy is called with the symlink path; it must resolve it before comparing/scanning.
	reason, err := InUseBy(link)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal: a process inside the symlink target must be detected via the symlink path")
	}
}

func TestInUseByCwdInsideRefuses(t *testing.T) {
	dest := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) { return nil, nil })
	t.Chdir(dest)
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason == "" {
		t.Fatal("want a refusal when the caller's own cwd is inside dest")
	}
	if !strings.Contains(reason, "cd out") {
		t.Errorf("reason = %q, want it to mention cd-ing out", reason)
	}
}

func TestInUseByCwdOutsideClear(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	stubProcesses(t, func(dir string) ([]Proc, error) { return nil, nil })
	t.Chdir(outside)
	reason, err := InUseBy(dest)
	if err != nil {
		t.Fatalf("InUseBy: %v", err)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want clear when cwd is outside dest", reason)
	}
}

// The one real-process integration test (Linux/macOS): a child with its cwd in a worktree is detected without stubs.
func TestInUseByRealProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("real-process detection is only implemented for linux and darwin")
	}
	t.Setenv("HOME", t.TempDir())
	useRealProcessScan(t)
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "busy", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "30")
	cmd.Dir = dest
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Poll briefly: the child's /proc entry (or lsof's view) can lag the Start() call by a beat.
	reason := pollInUse(t, dest, "InUseBy", func(r string) bool { return r != "" })
	if reason == "" {
		t.Fatal("want a refusal while the real sleep process has its cwd inside dest")
	}
	if !strings.Contains(reason, "in use by pid") {
		t.Errorf("reason = %q, want the pid-based in-use message", reason)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	_ = cmd.Wait()

	if reason = pollInUse(t, dest, "InUseBy after kill", func(r string) bool { return r == "" }); reason != "" {
		t.Fatalf("reason = %q, want clear after the process was killed", reason)
	}
}

// useRealProcessScan swaps the TestMain stubs for the OS process scan and settles InUse on the first look.
func useRealProcessScan(t *testing.T) {
	t.Helper()
	stubbedIn, stubbedUp, stubbedScan, origSettle := processesIn, parentOf, scanProcs, inUseSettle
	// TestMain stubbed the scan; this test needs the OS one
	if platformScan.procs != nil {
		processesIn, parentOf, scanProcs = nil, platformScan.up, platformScan.procs
	}
	// the sleep never exits on its own; refuse on the first look
	inUseSettle = 0
	t.Cleanup(func() { processesIn, parentOf, scanProcs, inUseSettle = stubbedIn, stubbedUp, stubbedScan, origSettle })
}

// pollInUse calls InUseBy(dest) for up to 5s until done(reason) holds and returns the last reason.
func pollInUse(t *testing.T, dest, label string, done func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var reason string
	for time.Now().Before(deadline) {
		var err error
		reason, err = InUseBy(dest)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if done(reason) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return reason
}

// PeerAgents ignores agents in a linked worktree, the caller's own agent ancestor and that agent's descendants.
func TestPeerAgentsIgnoresLinkedAndOwnTree(t *testing.T) {
	co, linked := "/co", "/co/wt"
	self := os.Getpid()
	stubProcesses(t, nil)
	origScan := scanProcs
	scanProcs = func() ([]procCwd, error) {
		return []procCwd{{100, co, "claude"}, {200, co, "codex"}, {300, linked, "claude"}, {400, co, "opencode"},
			{500, co, "vim"}, {600, co, "agy"}, {700, "/elsewhere", "claude"}}, nil
	}
	t.Cleanup(func() { scanProcs = origScan })
	origArgs := argvOf
	argvOf = func(pid int) ([]string, error) {
		if pid == 600 {
			// IDE hub daemon, not a working session
			return []string{"/home/u/.gemini/bin/agy", "--hub", "--hub-port=1"}, nil
		}
		return []string{"agent"}, nil
	}
	t.Cleanup(func() { argvOf = origArgs })
	parents := map[int]int{self: 100, 200: 100, 100: 1}
	parentOf = func(pid int) (int, error) { return parents[pid], nil }
	origName := commName
	commName = func(pid int) string {
		if pid == 100 {
			return "claude"
		}
		return ""
	}
	t.Cleanup(func() { commName = origName })
	got, err := PeerAgents(co, []string{linked})
	if err != nil || len(got) != 1 || got[0].PID != 400 {
		t.Fatalf("PeerAgents = %v, %v; want only pid 400", got, err)
	}
}

// PeerAgents walks the process table once however many linked worktrees there are, and still partitions them.
func TestPeerAgentsScansOnceWithLinkedDirs(t *testing.T) {
	co := "/co"
	linkedDirs := []string{"/co/wt1", "/co/wt2", "/co/wt3"}
	stubProcesses(t, nil)
	parentOf = func(pid int) (int, error) { return 0, nil }
	scans := 0
	origScan, origArgs, origName := scanProcs, argvOf, commName
	scanProcs = func() ([]procCwd, error) {
		scans++
		return []procCwd{{101, co, "claude"}, {102, "/co/wt1", "claude"}, {103, "/co/wt3/sub", "codex"},
			{104, "/co/.git/hooks", "claude"}, {105, "/co/src", "opencode"}, {106, "/elsewhere", "claude"}}, nil
	}
	argvOf = func(int) ([]string, error) { return []string{"agent"}, nil }
	commName = func(int) string { return "" }
	t.Cleanup(func() { scanProcs, argvOf, commName = origScan, origArgs, origName })
	got, err := PeerAgents(co, linkedDirs)
	if err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Errorf("scanProcs ran %d times, want 1", scans)
	}
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	if len(pids) != 2 || pids[0] != 101 || pids[1] != 105 {
		t.Errorf("peers = %v, want [101 105] (linked and .git dirs skipped)", pids)
	}
}

// A failed agent-only scan (denied sysctl, empty pid list) falls back to the full scan instead of reporting no peers.
func TestPeerAgentsFallsBackWhenAgentScanFails(t *testing.T) {
	co := "/co"
	stubProcesses(t, nil)
	parentOf = func(pid int) (int, error) { return 0, nil }
	origScan, origAgents, origArgs, origName := scanProcs, scanAgentProcs, argvOf, commName
	scanAgentProcs = func() ([]procCwd, error) { return nil, errors.New("sysctl denied") }
	scanProcs = func() ([]procCwd, error) {
		return []procCwd{{101, co, "claude"}, {102, "/co/src", "codex"}, {103, "/elsewhere", "claude"}}, nil
	}
	argvOf = func(int) ([]string, error) { return []string{"agent"}, nil }
	commName = func(int) string { return "" }
	t.Cleanup(func() { scanProcs, scanAgentProcs, argvOf, commName = origScan, origAgents, origArgs, origName })
	got, err := PeerAgents(co, nil)
	if err != nil || len(got) != 2 || got[0].PID != 101 || got[1].PID != 102 {
		t.Fatalf("PeerAgents = %v, %v; want pids 101 and 102 from the full scan", got, err)
	}
}

// darwin's scan parses `lsof -Fpcn`: a pid line resets the comm, and a cwd path with spaces survives intact.
func TestParseLsofCwd(t *testing.T) {
	out := "p101\ncclaude\nn/co/my repo\np202\nn/elsewhere\n\np303\nccodex\nn/co/wt\n"
	got, err := parseLsofCwd(out)
	want := []procCwd{{101, "/co/my repo", "claude"}, {202, "/elsewhere", ""}, {303, "/co/wt", "codex"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("parseLsofCwd = %+v, %v; want %+v", got, err, want)
	}
}

// agentComms keeps its five process names; an agentid rename fails here.
func TestAgentCommsTieToAgentid(t *testing.T) {
	want := map[string]bool{"claude": true, "codex": true, "copilot": true, "opencode": true, "agy": true}
	if !maps.Equal(agentComms, want) {
		t.Errorf("agentComms = %v, want %v", agentComms, want)
	}
}

// A busy worktree's refusal names the process and the next step; any non-excluded process still refuses (hookperm-20).
func TestInUseByRefusalNamesProcessAndNextStep(t *testing.T) {
	self := os.Getpid()
	cases := []struct {
		name  string
		procs []Proc
		want  string // "" = clear; else a substring of the reason
	}{
		{"foreign process names pid, dir and next step", []Proc{{PID: 4242, Comm: "sleep"}}, "then retry"},
		{"this session's own child still refuses", []Proc{{PID: 501, Comm: "go"}}, "in use by pid 501 (go)"},
		{"another session's agent still refuses", []Proc{{PID: 4343, Comm: "codex"}}, "in use by pid 4343 (codex)"},
		{"caller excluded", []Proc{{PID: self, Comm: "self"}}, ""},
		{"caller's agent ancestor excluded", []Proc{{PID: 100, Comm: "claude"}}, ""},
		{"ancestor listed before a foreign process", []Proc{{PID: 100, Comm: "claude"}, {PID: 4242, Comm: "sleep"}},
			"in use by pid 4242 (sleep)"},
		{"empty dir clear", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := t.TempDir()
			stubProcesses(t, func(string) ([]Proc, error) { return c.procs, nil })
			parents := map[int]int{self: 100, 100: 1, 501: 100}
			parentOf = func(pid int) (int, error) { return parents[pid], nil }
			reason, err := InUseBy(dest)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if reason != "" {
					t.Fatalf("reason = %q, want clear", reason)
				}
				return
			}
			if !strings.Contains(reason, c.want) || !strings.Contains(reason, paths.CanonDeep(dest)) ||
				!strings.Contains(reason, "stop it or wait for it to exit, then retry") {
				t.Fatalf("reason = %q, want %q, the dir and the next step", reason, c.want)
			}
		})
	}
}

// PeerAgents skips agents under this session's detached background jobs, and only those (hookperm-22).
func TestPeerAgentsSkipsOwnBackgroundJobs(t *testing.T) {
	co, linked := "/co", "/co/wt"
	self := os.Getpid()
	// self -> 100 (claude, this session); 900/910 are detached jobs reparented to 1, 900 owned by 100
	parents := map[int]int{self: 100, 100: 1, 800: 900, 900: 1, 810: 910, 910: 1, 820: 1, 830: 100, 840: 900}
	cases := []struct {
		name string
		proc procCwd
		want bool // reported as a peer
	}{
		{"agent under own background job", procCwd{800, co, "codex"}, false},
		{"agent under another session's job", procCwd{810, co, "codex"}, true},
		{"reparented agent with no job", procCwd{820, co, "claude"}, true},
		{"agent under own agent ancestor", procCwd{830, co, "opencode"}, false},
		{"own job's agent in a linked worktree", procCwd{840, linked, "codex"}, false},
		{"non-agent under nothing", procCwd{820, co, "vim"}, false},
		{"peer agent elsewhere", procCwd{820, "/elsewhere", "claude"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubProcesses(t, nil)
			parentOf = func(pid int) (int, error) { return parents[pid], nil }
			origScan, origArgs, origName, origJobs := scanProcs, argvOf, commName, ownJobs
			t.Cleanup(func() { scanProcs, argvOf, commName, ownJobs = origScan, origArgs, origName, origJobs })
			scanProcs = func() ([]procCwd, error) { return []procCwd{c.proc}, nil }
			argvOf = func(int) ([]string, error) { return []string{"agent"}, nil }
			commName = func(pid int) string {
				if pid == 100 {
					return "claude"
				}
				return ""
			}
			ownJobs = func(mine map[int]bool) map[int]bool {
				if mine[100] {
					return map[int]bool{900: true}
				}
				return nil
			}
			got, err := PeerAgents(co, []string{linked})
			if err != nil || (len(got) == 1) != c.want {
				t.Fatalf("PeerAgents = %v, %v; want reported=%v", got, err, c.want)
			}
		})
	}
}

// ownJobs keeps only live records owned by the caller's ancestry whose pid still has its recorded start time.
func TestOwnJobsFiltersRecords(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	self := os.Getpid()
	start, err := proc.StartTime(self)
	if err != nil {
		t.Skipf("process start time unavailable here: %v", err)
	}
	now := time.Now()
	recs := []agent.Record{
		{ID: "a-own", Status: "running", PID: self, PIDStart: start, Owner: 100, Time: now},
		{ID: "b-foreign", Status: "running", PID: self, PIDStart: start, Owner: 200, Time: now},
		{ID: "c-done", Status: "ok", PID: self, PIDStart: start, Owner: 100, Time: now},
		{ID: "d-reused", Status: "running", PID: self, PIDStart: "x" + start, Owner: 100, Time: now},
		{ID: "e-nopid", Status: "running", Owner: 100, Time: now},
		{ID: "f-init", Status: "running", PID: self, PIDStart: start, Owner: 1, Time: now},
	}
	for _, want := range []bool{true, false} {
		dir := paths.DispatchDir()
		_ = os.RemoveAll(dir)
		for _, r := range recs {
			if want == (r.ID == "a-own") {
				if err := r.Save(); err != nil {
					t.Fatal(err)
				}
			}
		}
		got := ownJobs(map[int]bool{self: true, 100: true, 1: true})
		if got[self] != want || len(got) > 1 {
			t.Errorf("with a-own saved=%v: ownJobs = %v, want self included=%v", want, got, want)
		}
	}
}
