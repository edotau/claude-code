// The relay poll gates: a relay's `agents result` poll is nudged to hand back, refused once reported or doubled.
package hooks

import (
	"bytes"
	"cmp"
	"context"
	"errors"
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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
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

// relayBash refuses a relay's Bash call that is not a claude-code, read-only or go gate command: the job does the work.
func relayBash(in *core.Input) (core.Result, bool) {
	cmd := in.Nested("tool_input", "command")
	if _, relay := relayLeaf(in); !relay || cmd == "" {
		return core.Result{}, false
	}
	for _, seg := range shell.Segments(cmd) {
		if rule := relayRefusal(seg); rule != "" {
			shown := textutil.Truncate(strings.Join(strings.Fields(textutil.RedactSecrets(seg)), " "), 60)
			return core.Result{Decision: core.Block, Rule: "relay-shell", Verdict: core.VerdictRefused, Reason: fmt.Sprintf("relay: refused `%s`: %s; run only "+
				"`claude-code <agent> ask ... - < /absolute/brief/file` (no cd: use --workdir), read-only commands "+
				"(git diff, grep, ls, cat), or: "+relayGoShapes+".", shown, rule)}, true
		}
	}
	return core.Result{}, false
}

// relayRefusal names the rule a relay's Bash segment breaks; "" when it is allowed.
func relayRefusal(seg string) string {
	if slices.ContainsFunc(shell.Tokens(seg), relayGoEnvRe().MatchString) {
		return "sets a Go environment variable (GOFLAGS, GOENV, GOWORK, GOBIN, GOCACHE or GOTOOLCHAIN)"
	}
	if toks := relayWords(seg); len(toks) == 0 || filepath.Base(toks[0]) != "claude-code" {
		if agent.ReadOnlyShell(seg) {
			return ""
		}
		if rule, isGo := relayGoGate(seg); isGo {
			return rule
		}
		return "not a `claude-code` command"
	}
	if cc := agentCommand(relayWords(seg)); len(cc) > 1 && cc[0] == "worktree" && cc[1] == "commit" {
		return "a relay does not commit; the dispatched agent or the main session commits"
	}
	_, _, err := relayInput(seg)
	if err != nil {
		return err.Error()
	}
	return ""
}

// relayGoEnvFlags write files or run other programs; a GOFLAGS carrying one is refused (leading dashes dropped).
var relayGoEnvFlags = sync.OnceValue(func() []string { return shell.GuardWords("relay_go_env_flags") })

// relayGoEnvRe is an inline assignment of a Go variable that can redirect what the toolchain runs or writes.
var relayGoEnvRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(` + strings.Join(shell.GuardWords("relay_go_env_vars"), "|") + `)=`)
})

// relayGoValue and relayGoBool are the flags a relay's go gate allows, by subcommand; a value flag takes one word.
var relayGoValue = sync.OnceValue(func() map[string][]string { return shell.GuardTools("relay_go_value_flags") })
var relayGoBool = sync.OnceValue(func() map[string][]string { return shell.GuardTools("relay_go_bool_flags") })

// relayGoShapes names the allowed go shapes in one line for the refusal message.
const relayGoShapes = "go vet|test <./packages> [-tags -p -race -v], go test also [-run -skip -count -timeout -shuffle -list " +
	"-short -failfast -cover -covermode -coverpkg -bench -benchmem -benchtime -parallel -cpu -vet -json], " +
	"go build ./... or go build <./packages> -o /dev/null, gofmt -l <./paths>"

// relayWrapRe is one leading wrapper in a safe shape; env takes no flag, and an assignment after it fails the go check.
var relayWrapRe = regexp.MustCompile(`^(?:rtk|env|(?:command )?time(?: -p)?|nice(?: -n -?[0-9]+)?|` +
	`g?timeout(?: -k [0-9]+[smh]?| --kill-after=[0-9]+[smh]?| -s [A-Za-z0-9]+| --signal=[A-Za-z0-9]+)*` +
	` [0-9]+[smh]?) `)

// relayUnwrap drops the leading wrappers relayWrapRe accepts, so the rest meets the checks of a bare command.
func relayUnwrap(toks []string) []string {
	for len(toks) > 1 {
		m := relayWrapRe.FindString(strings.Join(toks, " "))
		if m == "" {
			break
		}
		toks = toks[strings.Count(m, " "):]
	}
	return toks
}

// relayGoGate judges `go build|vet|test`, `gofmt -l` and their wrappers: !isGo for others, else rule is "" or why not.
func relayGoGate(seg string) (rule string, isGo bool) {
	toks := relayUnwrap(shell.Tokens(seg))
	if len(toks) < 2 || strings.ContainsAny(seg, "<>") || relayExpands(seg) {
		return "", false
	}
	if toks[0] == "gofmt" {
		return relayGofmtArgs(toks[1:]), true
	}
	if _, gated := relayGoValue()[toks[1]]; toks[0] != "go" || !gated { // the gated verbs are the value-flag table's
		return "", false
	}
	if rule := relayGoEnvRule(); rule != "" {
		return rule, true
	}
	return relayGoArgs(toks[1], toks[2:]), true
}

// relayExpands reports a backtick, `$(`, `${` or `$NAME` outside single quotes; a trailing `$` is literal.
func relayExpands(seg string) bool { return shell.Expands(seg) }

// relayGofmtArgs is "" for `gofmt -l` with relative paths inside the repository only.
func relayGofmtArgs(args []string) string {
	const bad = "gofmt is allowed only as `gofmt -l <relative paths inside the repository>`"
	if !slices.Contains(args, "-l") || len(args) < 2 {
		return bad
	}
	for _, a := range args {
		if a != "-l" && (strings.HasPrefix(a, "-") || !relayPathOK(a)) {
			return bad
		}
	}
	return ""
}

// relayPathOK is true for a relative path with no `..` element and no `~` or flag lead.
func relayPathOK(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "-") {
		return false
	}
	return !slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

// relayPkgOK is true for a `./` package pattern inside the repository.
func relayPkgOK(p string) bool { return strings.HasPrefix(p, "./") && relayPathOK(p) }

// relayGoEnvRule refuses a GOFLAGS this process sees with a refused flag; the relay shell's env is invisible.
func relayGoEnvRule() string {
	for f := range strings.FieldsSeq(os.Getenv("GOFLAGS")) {
		name, _, _ := strings.Cut(strings.TrimLeft(f, "-"), "=")
		if slices.Contains(relayGoEnvFlags(), name) {
			return "the inherited GOFLAGS carries -" + name
		}
	}
	return ""
}

// relayGoArgs is "" when args are in-repo package patterns and allowed flags only, and a build cannot drop a binary.
func relayGoArgs(sub string, args []string) string {
	var pkgs []string
	devNull := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			if !relayPkgOK(a) {
				return "package " + a + " must be a ./ pattern inside the repository"
			}
			pkgs = append(pkgs, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if slices.Contains(relayGoBool()[sub], name) {
			continue
		}
		if !slices.Contains(relayGoValue()[sub], name) {
			return "flag " + a + " is not allowed (or it writes files or runs another program)"
		}
		if !hasVal && i+1 < len(args) {
			i++
			val = args[i]
		} else if !hasVal {
			return "flag " + a + " needs a value"
		}
		if rule := relayFlagValue(sub, name, a, val); rule != "" {
			return rule
		}
		devNull = devNull || name == "o"
	}
	if sub == "build" && !devNull && !slices.Equal(pkgs, []string{"./..."}) {
		return "go build is allowed only as `go build ./...` or with `-o /dev/null`"
	}
	return ""
}

// relayFlagValue judges one flag's value: no leading dash, `-o` only /dev/null, `-coverpkg` in-repo patterns.
func relayFlagValue(sub, name, flag, val string) string {
	switch {
	case strings.HasPrefix(val, "-"):
		return "flag " + flag + " needs a value that does not begin with -"
	case sub == "build" && name == "o" && val != "/dev/null":
		return "go build -o is allowed only as `-o /dev/null`"
	case name == "coverpkg" && !slices.ContainsFunc(strings.Split(val, ","), func(p string) bool { return !relayPkgOK(p) }):
		return ""
	case name == "coverpkg":
		return "-coverpkg must list ./ package patterns inside the repository"
	}
	return ""
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

// reviewVerb is `claude-code <external agent> review`: a vendor run that is no ask, so no job id is waited on.
func reviewVerb(words []string) bool {
	words = agentCommand(words)
	return len(words) > 1 && externalTypes[words[0]] && words[1] == "review"
}

// relayInput permits only literal argv and one stdin file redirect; no expansions or shell execution.
func relayInput(seg string) (words []string, input string, err error) {
	if !relayQuotesClosed(seg) {
		return nil, "", errors.New("has an unclosed quote or escape")
	}
	at := shell.QuoteAt(seg)
	redirect := -1
	for i, c := range []byte(seg) {
		if at[i] == '\'' {
			continue
		}
		if c == '`' || c == '$' {
			return nil, "", errors.New("contains `$` or a backtick")
		}
		if at[i] == 0 && strings.ContainsRune(";|&(){}>*?[]~#", rune(c)) {
			return nil, "", fmt.Errorf("contains an unquoted shell operator `%c`", c)
		}
		if c == '<' && at[i] == 0 {
			if redirect >= 0 || i > 0 && !strings.ContainsRune(" \t", rune(seg[i-1])) {
				return nil, "", errors.New("redirect is not a literal `< file`")
			}
			redirect = i
		}
	}
	if redirect < 0 {
		return shell.Tokens(seg), "", nil
	}
	files := shell.Tokens(seg[redirect+1:])
	if len(files) != 1 || files[0] == "" {
		return nil, "", errors.New("redirect is not a literal `< file`")
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
