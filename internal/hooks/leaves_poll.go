// The relay poll gates: a relay's `agents result` poll is nudged to hand back, refused once reported or doubled.
package hooks

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/shell"
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
func pollJob(cmd string) (job string, follow, ok bool) {
	if !strings.Contains(cmd, "result") {
		return "", false, false
	}
	for _, seg := range shell.Segments(cmd) {
		toks, _ := shell.Unwrap(shell.Tokens(seg), "/")
		if len(toks) < 4 || filepath.Base(toks[0]) != "claude-code" || toks[1] != "agents" || toks[2] != "result" {
			continue
		}
		for _, a := range toks[3:] {
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

// relayLeaf is a codex|copilot|gemini|opencode leaf's own view of in, with its transcript path filled in.
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
	lr, job, own, follow, ok := relayPoll(in)
	if !ok {
		return core.Result{}
	}
	if own != "" && slices.Contains(lr.scan.reported, own) {
		if rec, err := loadDispatch(own); err == nil && !jobRunning(rec) {
			return core.Result{Decision: core.Block, Reason: fmt.Sprintf("already reported: stop. You handed back job %s after "+
				"it ended; do not poll it again, and answer any later notice about it with \"already reported\".", own)}
		}
	}
	if !follow {
		return core.Result{}
	}
	if where, live := livePoller(lr.scan.polls, in.Nested("tool_use_id"), cmp.Or(own, job), lr.ids); live {
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf("one poller only: an `agents result -f` call already "+
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
