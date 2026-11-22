// The relay poll gates: a relay's `agents result` poll is nudged to hand back, refused once reported or doubled.
package hooks

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
)

// pollCall is one Bash `agents result` call in a relay transcript and what its tool result showed.
type pollCall struct {
	job    string // the id argument as written, possibly a prefix
	follow bool   // -f/--follow: the call waits for the job
	done   bool   // its result came back in the foreground
	bgFile string // its result says it moved to the background, writing here
}

// jobArgRe is a dispatch id or its timestamp prefix; a substitution such as `$(ls …)` never matches.
var jobArgRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\d{8}T\d{6}\.\d{3}(?:-[A-Za-z0-9._-]+)?$`) })

// pollJob is the job id a command's `claude-code agents result [-f] <id>` segment names.
func pollJob(cmd string) (job string, follow, ok bool) { return verbJob(cmd, "result") }

// verbJob is the job id a command's `claude-code agents <verb> [flags] <id>` segment names.
func verbJob(cmd, verb string) (job string, follow, ok bool) {
	if !strings.Contains(cmd, verb) {
		return "", false, false
	}
	for _, toks := range relayCommands(cmd) {
		toks = agentCommand(toks)
		if len(toks) < 3 || toks[0] != "agents" || toks[1] != verb {
			continue
		}
		for _, a := range toks[2:] {
			switch {
			case a == "-f" || a == "--follow":
				follow = true
			case job == "" && jobArgRe().MatchString(a):
				job = a
			}
		}
		if job != "" {
			return job, follow, true
		}
	}
	return "", false, false
}

// ownJob is the relay's own job id that arg names (whole or as its prefix); "" when none does.
func ownJob(ids []string, arg string) string {
	if i := slices.IndexFunc(ids, func(id string) bool { return strings.HasPrefix(id, arg) }); i >= 0 {
		return ids[i]
	}
	return ""
}

// relayLeaf is an external relay's own view of in, with its transcript path filled in.
func relayLeaf(in *core.Input) (core.Input, bool) {
	id := in.SubagentID()
	if id == "" || in.Agent() != core.AgentClaude {
		return core.Input{}, false
	}
	leaf := *in
	leaf.AgentTranscriptPath = leafTranscript(in, id)
	return leaf, externalTypes[leafAgentType(&leaf)]
}

// leafTranscript is the leaf's transcript: the payload's path, else agent-<id>.jsonl in the session's subagents dir.
func leafTranscript(in *core.Input, id string) string {
	return cmp.Or(in.AgentTranscriptPath, filepath.Join(inflight.SubagentDir(in.TranscriptPath), "agent-"+id+".jsonl"))
}

// relayPoll is the relay's transcript read and its own job id for a Bash call that polls one; ok false otherwise.
func relayPoll(in *core.Input) (lr leafRead, job, own string, follow, ok bool) {
	job, follow, ok = pollJob(in.Nested("tool_input", "command"))
	if !ok {
		return lr, "", "", false, false
	}
	leaf, ok := relayLeaf(in)
	if !ok {
		return lr, "", "", false, false
	}
	lr = readLeaf(leaf.AgentTranscriptPath, leafWant{ids: true})
	if lr.openErr != nil || lr.scanErr != nil {
		return lr, "", "", false, false
	}
	return lr, job, ownJob(lr.ids, job), follow, true
}

// pollGate (PreToolUse Bash) refuses a poll of a finished job already handed back, and a second live poller.
func pollGate(in *core.Input) core.Result {
	if res, ok := relayBash(in); ok {
		return res
	}
	lr, job, own, follow, ok := relayPoll(in)
	if !ok {
		return core.Result{}
	}
	if own != "" && slices.Contains(lr.scan.reported, own) {
		if rec, err := loadDispatch(own); err == nil && !jobRunning(rec) {
			return core.Result{Decision: core.Block, Rule: "already-reported", Verdict: core.VerdictRefused, Reason: fmt.Sprintf("already reported: stop. You handed back job %s after "+
				"it ended; do not poll it again, and answer any later notice about it with \"already reported\".", own)}
		}
	}
	if !follow {
		return core.Result{}
	}
	if where, live := livePoller(lr.scan.polls, in.Nested("tool_use_id"), cmp.Or(own, job), lr.ids); live {
		return core.Result{Decision: core.Block, Rule: "one-poller", Verdict: core.VerdictRefused, Reason: fmt.Sprintf("one poller only: an `agents result -f` call already "+
			"follows job %s%s; wait for it to return, then hand back with its result.", cmp.Or(own, job), where)}
	}
	return core.Result{}
}

// livePoller finds another follow poll of job that is still waiting: in flight, or backgrounded with a live writer.
func livePoller(polls map[string]pollCall, self, job string, ids []string) (where string, live bool) {
	for id, p := range polls {
		if id == self || !p.follow || cmp.Or(ownJob(ids, p.job), p.job) != job {
			continue
		}
		if !p.done && p.bgFile == "" {
			return "", true
		}
		if p.bgFile != "" && len(reapHolders([]string{p.bgFile})) > 0 {
			return " (background output " + p.bgFile + ")", true
		}
	}
	return "", false
}

// pollNudge (PostToolUse Bash) tells a relay whose own poll just returned on an ended job to hand back now.
func pollNudge(in *core.Input) core.Result {
	if in.Nested("tool_response", "backgroundTaskId") != "" ||
		bgOutputPattern().MatchString(cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText())) {
		return core.Result{} // moved to the background: the poll has not returned
	}
	_, _, own, _, ok := relayPoll(in)
	if !ok || own == "" {
		return core.Result{}
	}
	if rec, err := loadDispatch(own); err != nil || jobRunning(rec) {
		return core.Result{}
	}
	return core.Result{Context: fmt.Sprintf("job %s ended: hand back now with this result", own)}
}

// AfterTool is dispatch-workflow: a Workflow releases its slots; Bash marks read jobs collected and nudges a relay.
func AfterTool(in *core.Input) core.Result {
	if in.ToolName == "Bash" {
		return core.WithStderr(pollNudge(in), markCollected(in))
	}
	return Release(in)
}

const reapGrace = 2 * time.Second

// Seams for tests: the process finder and the sleep; the group probe/killer live in leaves_{unix,other}.go.
var (
	reapHolders = outputHolders
	reapSleep   = time.Sleep
)

// appendOutputFiles adds the task output files one transcript line's Bash tool results named that seen lacks.
func appendOutputFiles(files []string, seen map[string]bool, b []byte) []string {
	if !bytes.Contains(b, []byte(`"tool_result"`)) {
		return files
	}
	for _, m := range bgOutputPattern().FindAllSubmatch(b, -1) {
		if f := string(m[1]); !seen[f] {
			seen[f] = true
			files = append(files, f)
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
		return lsofHolders(files)
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

// lsofHolders is outputHolders off Linux: one lsof over the files that still exist, none when no file is left.
func lsofHolders(files []string) []int {
	files = slices.DeleteFunc(slices.Clone(files), func(f string) bool { _, err := os.Stat(f); return err != nil })
	if len(files) == 0 {
		return nil // a removed file has no holder lsof can name: skip the exec
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := append([]string{"-a", "-d", "1,2", "-t", "--"}, files...) // stdout/stderr only, like /proc
	out, err := exec.CommandContext(ctx, "lsof", args...).Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	return parsePids(out)
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
