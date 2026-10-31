//go:build unix

// The leaf-stop reaper: after a stop the gate lets through, kill the leaf's own leftover background shells.
package hooks

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// bgOutputPattern matches the Bash run_in_background result; Monitor results name no output path, so they are not reaped.
var bgOutputPattern = regexp.MustCompile(`Command running in background with ID: [A-Za-z0-9]+\. Output is being written to: (/[^\s"]+?/tasks/[A-Za-z0-9]+\.output)\.`)

const reapGrace = 2 * time.Second

// Seams for tests: the process finder, the group probe/killer and the sleep.
var (
	reapHolders = outputHolders
	reapGroupOf = syscall.Getpgid
	reapSignal  = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
	reapSleep   = time.Sleep
)

func init() { reapLeaf = reapLeafBackground }

// leafOutputFiles lists the task output files the leaf's own Bash tool results named, from its transcript.
func leafOutputFiles(transcript string) []string {
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()
	var files []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"tool_result"`) {
			continue
		}
		for _, m := range bgOutputPattern.FindAllStringSubmatch(line, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				files = append(files, m[1])
			}
		}
	}
	return files
}

// outputHolders returns the pids whose stdout or stderr is the given file; every error yields none.
func outputHolders(path string) []int {
	if runtime.GOOS != "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "lsof", "-a", "-d", "1,2", "-t", "--", path).Output() // stdout/stderr only, like /proc
		if err != nil && len(out) == 0 {
			return nil
		}
		return parsePids(out)
	}
	var pids []int
	for _, fd := range []string{"1", "2"} {
		links, _ := filepath.Glob("/proc/[0-9]*/fd/" + fd)
		for _, l := range links {
			if dst, err := os.Readlink(l); err == nil && dst == path {
				if pid, err := strconv.Atoi(strings.Split(l, "/")[2]); err == nil {
					pids = append(pids, pid)
				}
			}
		}
	}
	return pids
}

func parsePids(out []byte) []int {
	var pids []int
	for f := range bytes.FieldsSeq(out) {
		if pid, err := strconv.Atoi(string(f)); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// reapGroups maps holder pids to distinct process groups, never our own or the parent's (the Claude session's).
func reapGroups(pids []int) []int {
	protected := map[int]bool{syscall.Getpgrp(): true}
	if g, err := reapGroupOf(os.Getppid()); err == nil {
		protected[g] = true
	}
	var groups []int
	seen := map[int]bool{}
	for _, pid := range pids {
		g, err := reapGroupOf(pid)
		if err != nil || g <= 1 || pid == os.Getpid() || protected[g] || seen[g] {
			continue
		}
		seen[g] = true
		groups = append(groups, g)
	}
	return groups
}

// terminateGroups sends SIGTERM, then SIGKILL after the grace period to any group still alive.
func terminateGroups(groups []int) {
	for _, g := range groups {
		_ = reapSignal(g, syscall.SIGTERM)
	}
	for waited := time.Duration(0); waited < reapGrace; waited += 100 * time.Millisecond {
		if !anyGroupAlive(groups) {
			return
		}
		reapSleep(100 * time.Millisecond)
	}
	for _, g := range groups {
		_ = reapSignal(g, syscall.SIGKILL)
	}
}

func anyGroupAlive(groups []int) bool {
	for _, g := range groups {
		if reapSignal(g, 0) == nil {
			return true
		}
	}
	return false
}

// reapLeafBackground kills the process groups writing to the task outputs of the stopping leaf's own transcript.
func reapLeafBackground(in *Input) {
	if in.AgentTranscriptPath == "" || !proc.Alive(os.Getpid()) {
		return
	}
	var pids []int
	for _, f := range leafOutputFiles(in.AgentTranscriptPath) {
		pids = append(pids, reapHolders(f)...)
	}
	if groups := reapGroups(pids); len(groups) > 0 {
		terminateGroups(groups)
		fmt.Fprintf(os.Stderr, "dispatch-stop: reaped %d leftover background group(s)\n", len(groups))
	}
}
