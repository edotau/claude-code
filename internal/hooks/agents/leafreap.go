// The leaf-stop reaper: after a stop the gate lets through, kill the leaf's own leftover background shells.
package agents

import (
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
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// bgOutputPattern matches the Bash run_in_background result; Monitor results name no output path, so they are not reaped.
var bgOutputPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`Command running in background with ID: [A-Za-z0-9]+\. Output is being written to: (/[^\s"]+?/tasks/[A-Za-z0-9]+\.output)\.`)
})

const reapGrace = 2 * time.Second

// Seams for tests: the process finder and the sleep; the group probe/killer live in leafreap_{unix,other}.go.
var (
	reapHolders = outputHolders
	reapSleep   = time.Sleep
)

// appendOutputFiles adds the task output files one transcript line's Bash tool results named that seen lacks.
func appendOutputFiles(files []string, seen map[string]bool, b []byte) []string {
	if !bytes.Contains(b, []byte(`"tool_result"`)) {
		return files
	}
	line := string(b)
	for _, m := range bgOutputPattern().FindAllStringSubmatch(line, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			files = append(files, m[1])
		}
	}
	return files
}

// outputHolders returns the pids whose stdout or stderr is one of the files, in a single scan; every error yields none.
func outputHolders(files []string) []int {
	if len(files) == 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		args := append([]string{"-a", "-d", "1,2", "-t", "--"}, files...) // stdout/stderr only, like /proc
		out, err := exec.CommandContext(ctx, "lsof", args...).Output()
		if err != nil && len(out) == 0 {
			return nil
		}
		return parsePids(out)
	}
	want := map[string]bool{}
	for _, f := range files {
		if real, err := filepath.EvalSymlinks(f); err == nil {
			f = real // /proc names the resolved path, so a symlinked $TMPDIR still matches
		}
		want[f] = true
	}
	var pids []int
	for _, fd := range []string{"1", "2"} {
		links, _ := filepath.Glob("/proc/[0-9]*/fd/" + fd)
		for _, l := range links {
			if dst, err := os.Readlink(l); err == nil && want[strings.TrimSuffix(dst, " (deleted)")] {
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
	protected := map[int]bool{reapOwnGroup(): true}
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
func reapLeafBackground(in *core.Input, lr leafRead) {
	if in.AgentTranscriptPath == "" {
		return
	}
	if groups := reapGroups(reapHolders(lr.files)); len(groups) > 0 {
		terminateGroups(groups)
		fmt.Fprintf(os.Stderr, "dispatch-stop: reaped %d leftover background group(s)\n", len(groups))
	}
}
