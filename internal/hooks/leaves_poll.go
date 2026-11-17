// The relay poll gates: a relay's `agents result` poll is nudged to hand back, refused once reported or doubled.
package hooks

import (
	"cmp"
	"errors"
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
	for _, toks := range relayCommands(cmd) {
		toks = agentCommand(toks)
		if len(toks) < 3 || toks[0] != "agents" || toks[1] != "result" {
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

// relayBash refuses a relay's Bash call that is not a claude-code command: the job does the work, not the relay.
func relayBash(in *core.Input) (core.Result, bool) {
	cmd := in.Nested("tool_input", "command")
	if _, relay := relayLeaf(in); !relay || cmd == "" {
		return core.Result{}, false
	}
	for _, seg := range shell.Segments(cmd) {
		toks := relayWords(seg)
		_, _, err := relayInput(seg)
		if err != nil || len(toks) == 0 || filepath.Base(toks[0]) != "claude-code" {
			return core.Result{Decision: core.Block, Reason: "relay: run only `claude-code` commands (your brief's " +
				"`Run:` line, then `claude-code agents result -f <id>`); the job reads, edits and tests. " +
				"Pass --workdir instead of cd; only a literal stdin file redirect (`< file`) is allowed."}, true
		}
	}
	return core.Result{}, false
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

// relayWords shares wrapper handling without changing the safety scanner's Unwrap behavior.
func relayWords(seg string) []string {
	words := shell.Tokens(seg)
	for {
		words, _ = shell.Unwrap(words, "/")
		if len(words) == 0 || filepath.Base(words[0]) != "rtk" {
			return words
		}
		words = words[1:]
	}
}

func relayCommands(cmd string) [][]string {
	var out [][]string
	for _, seg := range shell.Segments(cmd) {
		out = append(out, relayWords(seg))
	}
	return out
}

func agentCommand(words []string) []string {
	if len(words) < 3 || filepath.Base(words[0]) != "claude-code" {
		return nil
	}
	words = words[1:]
	if len(words) > 2 && words[0] == "--remote" {
		words = words[2:]
	}
	return words
}

func askVerb(words []string) string {
	words = agentCommand(words)
	if len(words) < 2 {
		return ""
	}
	if words[0] == "agents" && slices.Contains([]string{"ask", "dispatch", "result"}, words[1]) {
		return words[1]
	}
	if externalTypes[words[0]] && words[1] == "ask" {
		return "ask"
	}
	return ""
}

// relayInput permits only literal argv and one stdin file redirect; no expansions or shell execution.
func relayInput(seg string) (words []string, input string, err error) {
	if !relayQuotesClosed(seg) {
		return nil, "", errors.New("unclosed shell quote or escape")
	}
	at := shell.QuoteAt(seg)
	redirect := -1
	for i, c := range []byte(seg) {
		if at[i] == '\'' {
			continue
		}
		if c == '`' || c == '$' || at[i] == 0 && strings.ContainsRune(";|&(){}>*?[]~#", rune(c)) {
			return nil, "", errors.New("unsupported shell syntax")
		}
		if c == '<' && at[i] == 0 {
			if redirect >= 0 || i > 0 && !strings.ContainsRune(" \t", rune(seg[i-1])) {
				return nil, "", errors.New("unsupported redirect")
			}
			redirect = i
		}
	}
	if redirect < 0 {
		return shell.Tokens(seg), "", nil
	}
	files := shell.Tokens(seg[redirect+1:])
	if len(files) != 1 || files[0] == "" {
		return nil, "", errors.New("stdin redirect needs one literal file")
	}
	return shell.Tokens(seg[:redirect]), files[0], nil
}

// askCmdRe retains the opening-line match API while checking command position with the shared parser.
func askCmdRe() askMatcher { return askMatcher{} }

type askMatcher struct{}

func (askMatcher) MatchString(cmd string) bool {
	for _, words := range relayCommands(cmd) {
		if askVerb(words) != "" {
			return true
		}
	}
	return false
}

func (askMatcher) FindStringSubmatchIndex(cmd string) []int {
	offset := 0
	for _, seg := range shell.Segments(cmd) {
		words := relayWords(seg)
		if askVerb(words) != "" {
			all := shell.Tokens(seg)
			skip := len(all) - len(words)
			spans := shellWordRe().FindAllStringIndex(seg, -1)
			if skip >= 0 && skip < len(spans) {
				return []int{offset, offset + len(seg), offset + spans[skip][0], offset + len(seg)}
			}
		}
		offset += len(seg) + 1
	}
	return nil
}

func relayQuotesClosed(seg string) bool {
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '\\' && quote != '\'' {
			i++
			if i == len(seg) {
				return false
			}
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
		}
	}
	return quote == 0
}
