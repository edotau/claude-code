package wthook

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/worktree"
)

// claimAlive probes a claims file's owner pid; a var seam so tests decide which owners are live.
var claimAlive = proc.Alive

// claimShown caps how many claimed paths one ask names.
const claimShown = 5

// gitStageCmd pre-screens a Bash command for a git add/commit before any claims file is read.
var gitStageCmd = core.Gate(`\bgit\b[^;&|\n]*\b(?:add|commit)\b`, "git")

// claimHit is one live claim on a path by someone other than the caller: who, and their newest edit's time.
type claimHit struct {
	Who string
	TS  int64
}

// liveClaims maps each path other live parties claim to its claimants; a dead owner pid's file is removed.
func liveClaims(in *core.Input) (map[string][]claimHit, error) {
	self := sessionSlug(in.SessionID)
	leaf := siblingLeaf(in)
	own := sync.OnceValue(claimOwner) // looked up once, and only when another session's file is seen
	labels := map[int]string{}
	byPath := map[string]map[string]int64{}
	err := worktree.ScanClaims(claimAlive, func(pid int, slug string, c worktree.Claim) {
		if c.Path == "" {
			return
		}
		w := leaf(c) // own session: only a sibling leaf's claim counts
		// our own pid under another slug is /clear or a resume, not a peer
		if slug != self && pid != own() {
			if labels[pid] == "" {
				labels[pid] = SessionLabel(worktree.Proc{PID: pid, Comm: "claude"})
			}
			w = labels[pid] // another session: every claim counts while its owner lives
		}
		if w == "" {
			return
		}
		if byPath[c.Path] == nil {
			byPath[c.Path] = map[string]int64{}
		}
		byPath[c.Path][w] = max(byPath[c.Path][w], c.TS)
	})
	return flattenClaims(byPath), err
}

// siblingLeaf names another leaf's claim in this session's file while its transcript is fresh; own and main's yield "".
func siblingLeaf(in *core.Input) func(worktree.Claim) string {
	live := map[string]bool{}
	return func(c worktree.Claim) string {
		if c.Agent == "" || c.Agent == in.AgentID {
			return ""
		}
		ok, seen := live[c.Agent]
		if !seen {
			tr := filepath.Join(inflight.SubagentDir(in.TranscriptPath), "agent-"+c.Agent+".jsonl")
			st, err := os.Stat(tr)
			ok = err == nil && time.Since(st.ModTime()) < inflight.Idle && !leafEnded(tr)
			live[c.Agent] = ok
		}
		if !ok {
			return ""
		}
		return "leaf " + c.Agent[:min(8, len(c.Agent))]
	}
}

// leafEnded: the leaf's last assistant turn ended or handed back undenied, or the user interrupted it after.
func leafEnded(transcript string) bool {
	lines, ok := tailLines(transcript)
	if !ok {
		return false
	}
	failed := map[string]bool{} // tool_use_id → is_error, for results after the last assistant turn
	later := false              // a record follows the last assistant turn
	for _, line := range slices.Backward(lines) {
		var rec leafRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Type == "" {
			continue
		}
		if rec.Type == "assistant" {
			return turnEnded(rec, failed, later)
		}
		later = true
		for _, c := range rec.blocks() {
			if c.interrupted() {
				return true
			}
			if c.Type == "tool_result" {
				failed[c.ToolUseID] = c.IsError
			}
		}
	}
	return false
}

// tailLines is the last leafTail bytes of transcript split into lines; false when it cannot be opened.
func tailLines(transcript string) ([]string, bool) {
	f, err := os.Open(transcript)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > leafTail {
		_, _ = f.Seek(-leafTail, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return strings.Split(strings.TrimSpace(string(b)), "\n"), true
}

// turnEnded: an ended turn (a max_tokens cut only as the last record; recovery appends one), or an undenied hand-back.
func turnEnded(rec leafRecord, failed map[string]bool, later bool) bool {
	switch rec.Message.StopReason {
	case "end_turn":
		return true
	case "max_tokens", "stop_sequence":
		if !later {
			return true
		}
	}
	return slices.ContainsFunc(rec.blocks(), func(c leafContent) bool {
		isErr, seen := failed[c.ID]
		return c.Type == "tool_use" && c.Name == "SubagentHandback" && seen && !isErr
	})
}

// leafRecord is the slice of a transcript line leafEnded reads; a denied tool call's result carries is_error.
type leafRecord struct {
	Type    string `json:"type"`
	Message struct {
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"` // a block list, or a plain string prompt
	} `json:"message"`
}

// blocks is the record's content blocks; a plain string prompt has none.
func (r leafRecord) blocks() []leafContent {
	var out []leafContent
	_ = json.Unmarshal(r.Message.Content, &out)
	return out
}

type leafContent struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
	Content   json.RawMessage `json:"content"`
}

// interruptMark opens the text Claude Code records when the user interrupts a turn or its tool call.
const interruptMark = "[Request interrupted by user"

// interrupted reports the user's interrupt: its text block, or an errored tool result carrying the mark.
func (c leafContent) interrupted() bool {
	if c.Type == "text" {
		return strings.HasPrefix(c.Text, interruptMark)
	}
	if c.Type != "tool_result" || !c.IsError {
		return false
	}
	var s string
	if json.Unmarshal(c.Content, &s) == nil {
		return strings.HasPrefix(s, interruptMark)
	}
	var parts []leafContent
	_ = json.Unmarshal(c.Content, &parts)
	return slices.ContainsFunc(parts, func(p leafContent) bool { return p.interrupted() })
}

// leafTail bounds how much of a leaf transcript leafEnded reads from its end.
const leafTail = 256 << 10

func flattenClaims(byPath map[string]map[string]int64) map[string][]claimHit {
	out := make(map[string][]claimHit, len(byPath))
	for p, whos := range byPath {
		for w, ts := range whos {
			out[p] = append(out[p], claimHit{Who: w, TS: ts})
		}
		slices.SortFunc(out[p], func(a, b claimHit) int { return strings.Compare(a.Who, b.Who) })
	}
	return out
}

// claimView is one check's inputs: the hook payload, its deadline, the live claims and this session's clean marks.
type claimView struct {
	ctx    context.Context
	in     *core.Input
	claims map[string][]claimHit
	clean  func() map[string]int64
	status map[string]dirtyState // dirtySet per toplevel: the tree is unchanged until the checked command runs
}

// dirtyState is one dirtySet answer.
type dirtyState struct {
	top   string
	dirty map[string]bool
	err   error
}

// dirtyAt is dirtySet(dir), asked of git once per toplevel for this check.
func (v claimView) dirtyAt(dir string) (string, map[string]bool, error) {
	top, _ := paths.CheckoutOf(dir)
	st, ok := v.status[top]
	if !ok {
		st.top, st.dirty, st.err = dirtySet(v.ctx, dir)
		v.status[top] = st
	}
	return st.top, st.dirty, st.err
}

// ClaimCheck notes a dirty claimed file before an edit and asks (a leaf blocks) when git stages one.
func ClaimCheck(in *core.Input) core.Result {
	cmd := in.ToolCommand()
	bash := in.ToolName == "Bash"
	git := gitAnyCmd().MatchString(cmd)
	if in.SessionID == "" || bash && !git && !core.MayWrite(cmd) {
		return core.Result{}
	}
	edits := editPaths(in)
	ctx, cancel := core.HookContext("worktree-claim-check")
	defer cancel()
	if !bash {
		return claimResult(ctx, in, edits)
	}
	res := core.Result{}
	if gitStageCmd.MatchString(cmd) || len(edits) > 0 {
		res = claimResult(ctx, in, edits)
	}
	if !git {
		return res
	}
	notes, warn := noticeContext(ctx, in, newRun(ctx, in))
	res.Context = core.JoinContext(notes, res.Context)
	res.Stderr = strings.TrimSpace(warn + "\n" + res.Stderr)
	return res
}

// claimResult runs the edit check on each edited path and, for Bash, the stage check, against the live claims.
func claimResult(ctx context.Context, in *core.Input, edits []string) core.Result {
	claims, err := liveClaims(in) // on error, the claims from the readable files still come back
	res := core.Result{}
	if len(claims) > 0 {
		v := claimView{ctx: ctx, in: in, claims: claims, status: map[string]dirtyState{},
			clean: sync.OnceValue(func() map[string]int64 { return cleanMarks(in.SessionID) })}
		if in.ToolName == "Bash" && gitStageCmd.MatchString(in.ToolCommand()) {
			res = v.stageCheck()
		}
		for _, p := range edits {
			r := v.editCheck(p)
			res.Context = core.JoinContext(res.Context, r.Context)
			res.Stderr = strings.TrimSpace(res.Stderr + "\n" + r.Stderr)
		}
	}
	if err != nil {
		res.Stderr = strings.TrimSpace(claimWarn(err).Stderr + "\n" + res.Stderr)
	}
	return res
}

// claimWarn is the fail-open result for a check that could not run.
func claimWarn(err error) core.Result {
	return core.Result{Stderr: fmt.Sprintf("worktree-claim-check: WARNING — could not check file claims (%v); allowed", err)}
}

// editPaths are the files the call edits, absolute and canonical: a Write/Edit target, or what a Bash command writes.
func editPaths(in *core.Input) []string {
	if in.ToolName == "Bash" {
		out := core.WrittenPaths(in.ToolCommand(), in.CWD)
		for i, p := range out {
			out[i] = paths.Real(p)
		}
		return out
	}
	if p := in.Nested("tool_input", "file_path"); p != "" {
		return []string{paths.Real(core.AnchorPath(p, in.CWD))}
	}
	return nil
}

// editCheck returns the once-per-(session, path, who) advisory for an edit of a dirty claimed file.
func (v claimView) editCheck(p string) core.Result {
	if len(v.claims[p]) == 0 {
		return core.Result{}
	}
	top, dirty, err := v.dirtyAt(filepath.Dir(p))
	if err != nil {
		return claimWarn(err)
	}
	// only a claim the last mark left unretired needs a new one: one line per peer edit, not per own edit
	if top != "" && !dirty[p] && slices.ContainsFunc(v.claims[p], func(h claimHit) bool { return h.TS >= v.cleanAt(p) }) {
		markClean(v.in.SessionID, p) // nobody's hunks are left in p: claims made before now are retired
	}
	hits, err := v.freshHits(top, p, dirty)
	if err != nil {
		return claimWarn(err)
	}
	rel, _ := core.RelWithin(top, p)
	var lines []string
	for _, h := range hits {
		if firstSighting(v.in.SessionID, p+"\t"+h.Who) {
			lines = append(lines, fmt.Sprintf("%s has uncommitted edits by %s (since %s); edit only your own hunks "+
				"and stage just those (%s), or hand the change to its owner.", cmp.Or(rel, p), h.Who, clock(h.TS),
				ownHunks))
		}
	}
	return core.Result{Context: strings.Join(lines, "\n")}
}

// ownHunks is the non-interactive way to stage part of a file; `git add -p` waits on a prompt no hook shell answers.
const ownHunks = "save `git diff <path>` to a patch file outside the repo, delete the hunks that are not yours, " +
	"then `git apply --cached <patch>`"

// cleanMark prefixes a seen-store line recording that a path was clean (path, then unix time, tab-separated).
const cleanMark = "clean\t"

// markClean records in session's seen store that p is clean now; a store error drops it (the mark is advisory).
func markClean(session, p string) {
	_, _ = core.SeenAdd(seenKind, session, cleanMark+p+"\t"+strconv.FormatInt(time.Now().Unix(), 10))
}

// cleanMarks maps each path session saw clean to the latest time it did.
func cleanMarks(session string) map[string]int64 {
	b, _ := os.ReadFile(seenFile(session))
	out := map[string]int64{}
	for line := range strings.Lines(string(b)) {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), cleanMark)
		i := strings.LastIndexByte(rest, '\t')
		if !ok || i < 0 {
			continue
		}
		if ts, err := strconv.ParseInt(rest[i+1:], 10, 64); err == nil {
			out[rest[:i]] = max(out[rest[:i]], ts)
		}
	}
	return out
}

// dirtySet is dir's repo toplevel and its dirty paths (absolute, untracked included); "" top outside a repo.
func dirtySet(ctx context.Context, dir string) (string, map[string]bool, error) {
	top, _ := paths.CheckoutOf(dir)
	if top == "" {
		return "", nil, nil
	}
	top = paths.Real(top)
	raw, err := proc.RunGit(ctx, top, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return "", nil, fmt.Errorf("git status in %s: %w", top, err)
	}
	dirty := map[string]bool{}
	for _, e := range proc.StatusEntries(raw) {
		dirty[filepath.Join(top, e.Path)] = true
		if e.From != "" {
			dirty[filepath.Join(top, e.From)] = true // a rename or copy source is dirty too
		}
	}
	return top, dirty, nil
}

// freshHits keeps p's claims when p is dirty, dropping those older than p's last commit or this session's clean mark.
func (v claimView) freshHits(top, p string, dirty map[string]bool) ([]claimHit, error) {
	if top == "" || !dirty[p] {
		return nil, nil
	}
	out, _, err := core.Git(v.ctx, top, "log", "-1", "--format=%ct", "--", p)
	if err != nil {
		return nil, err
	}
	var landed int64
	if len(out) > 0 {
		landed, _ = strconv.ParseInt(out[0], 10, 64)
	}
	landed = max(landed, v.cleanAt(p))
	return slices.DeleteFunc(slices.Clone(v.claims[p]), func(h claimHit) bool { return h.TS < landed }), nil
}

// cleanAt is this session's latest clean mark on p; 0 when none.
func (v claimView) cleanAt(p string) int64 {
	if v.clean == nil {
		return 0
	}
	return v.clean()[p]
}

// seenKind is the shared seen store's kind for the busy, claim and divergence notices.
const seenKind = "worktree-busy"

func seenFile(session string) string { return core.SeenFile(seenKind, session) }

// firstSighting reports whether key is new to this session, warning on stderr and failing open on a store error.
func firstSighting(session, key string) bool {
	first, err := core.SeenAdd(seenKind, session, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worktree-claim-check: seen store: %v\n", err)
	}
	return first
}

func clock(ts int64) string { return time.Unix(ts, 0).Format("15:04") }

// stageCheck asks (a leaf: blocks) when a git add/commit in the command would stage a dirty claimed file.
func (v claimView) stageCheck() core.Result {
	var res core.Result
	var warns []string // a fail-open warning must not hide a later invocation in the chain
	// A data heredoc body (file sink, script interpreter) stages nothing.
	cmd := dropScriptHeredocs(core.DropDataHeredocs(v.in.ToolCommand()))
	shell.EachGit(cmd, v.in.CWD, func(target, verb string, args []string) bool {
		res = v.stageVerdict(target, verb, args)
		if verb == shell.Uninspectable {
			res = v.uninspectableVerdict()
			if shell.TooDeep(args) && !stageWord.MatchString(cmd) {
				res = core.Result{}
			}
		}
		if res.Decision == core.Allow && res.Stderr != "" {
			warns = append(warns, res.Stderr)
			res = core.Result{}
		}
		return res.Decision != core.Allow
	})
	if len(warns) > 0 {
		res.Stderr = strings.TrimSpace(strings.Join(warns, "\n") + "\n" + res.Stderr)
	}
	return res
}

// stageWord matches the git verbs stageVerdict checks, as a whole word, in text too deep to inspect.
var stageWord = core.Gate(`\b(?:add|commit)\b`, "add", "commit")

// scriptReader names a non-shell interpreter whose heredoc body is its own script, never shell commands.
var scriptReader = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(python[0-9.]*|node|ruby|perl|php|deno|bun)$`)
})

// dropScriptHeredocs removes heredoc bodies a script interpreter reads; an unquoted body with `$` or a backtick stays.
func dropScriptHeredocs(cmd string) string {
	return core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		m := core.HeredocOp().FindAllStringSubmatch(lines[i], -1)
		if len(m) != 1 || !scriptHeredoc(lines[i]) {
			return -1
		}
		end := core.HeredocEnd(lines, i+1, m[0][2]+m[0][3]+m[0][4], m[0][1] == "-")
		if end < 0 || m[0][4] != "" && strings.ContainsAny(strings.Join(lines[i+1:end], "\n"), "$`") {
			return -1
		}
		return end
	})
}

// scriptHeredoc reports a line whose heredoc segment runs a script interpreter and is not piped on.
func scriptHeredoc(line string) bool {
	if strings.Contains(line, "<<<") || strings.Contains(line, ">(") || strings.Contains(line, "<(") {
		return false
	}
	segs, seps := shell.SegmentsSep(line)
	for i, seg := range segs {
		if !core.HeredocOp().MatchString(seg) {
			continue
		}
		rest, _ := shell.Unwrap(shell.Tokens(core.HeredocOp().ReplaceAllString(seg, " ")), "")
		return len(rest) > 0 && scriptReader().MatchString(filepath.Base(rest[0])) && !strings.HasPrefix(seps[i], "|")
	}
	return false
}

// uninspectableVerdict asks (a leaf: blocks) on a command nested too deep to see what it stages.
func (v claimView) uninspectableVerdict() core.Result {
	d := core.Ask
	if v.in.AgentID != "" {
		d = core.Block
	}
	return core.Result{Decision: d, Reason: core.SafetyMessage(v.in, d, "command nests too deep to check what it stages", "",
		"unwrap the nested bash -c/eval and run the git add/commit directly")}
}

// stageVerdict checks one git invocation's staged set against the claims.
func (v claimView) stageVerdict(target, verb string, args []string) core.Result {
	w := splitStage(verb, args)
	if verb != "add" && verb != "commit" || interactive(verb, w.flags) || stagesNothing(verb, w.flags) {
		return core.Result{}
	}
	top, dirty, err := v.dirtyAt(target)
	if err != nil {
		return claimWarn(err)
	}
	if top == "" {
		return core.Result{}
	}
	scopes, err := stageScopes(v.ctx, top, target, verb, args)
	if err != nil {
		return claimWarn(err)
	}
	if w.trackedOnly(verb) {
		if dirty, err = tracked(v.ctx, top, dirty); err != nil {
			return claimWarn(err)
		}
	}
	taken, claimed, err := v.takenPaths(top, dirty, scopes)
	if err != nil {
		return claimWarn(err)
	}
	return stageResult(v.in, verb, taken, stageCount{total: countUnder(dirty, scopes), claimed: claimed})
}

// stageCount sizes a stage: files the git call takes, and how many of them another owner claims.
type stageCount struct{ total, claimed int }

// countUnder counts the dirty paths under scopes, i.e. the files a git add/commit would take.
func countUnder(dirty map[string]bool, scopes []string) int {
	n := 0
	for p := range dirty {
		if underAny(p, scopes) {
			n++
		}
	}
	return n
}

// takenPaths names each dirty claimed path under scopes with its claimants, plus the distinct claimed paths.
func (v claimView) takenPaths(top string, dirty map[string]bool, scopes []string) ([]string, int, error) {
	var taken []string
	claimed := 0
	for p := range v.claims {
		if !underAny(p, scopes) {
			continue
		}
		hits, err := v.freshHits(top, p, dirty)
		if err != nil {
			return nil, 0, err
		}
		if len(hits) > 0 {
			claimed++
		}
		rel, _ := core.RelWithin(top, p)
		for _, h := range hits {
			taken = append(taken, fmt.Sprintf("%s takes %s's uncommitted hunks (since %s)", cmp.Or(rel, p), h.Who,
				clock(h.TS)))
		}
	}
	return taken, claimed, nil
}

// stageResult renders the ask (main) or block (leaf) for the claimed paths, capped at claimShown.
func stageResult(in *core.Input, verb string, taken []string, n stageCount) core.Result {
	if len(taken) == 0 {
		return core.Result{}
	}
	slices.Sort(taken)
	more := ""
	if len(taken) > claimShown {
		more = fmt.Sprintf("; and %d more", len(taken)-claimShown)
		taken = taken[:claimShown]
	}
	d := core.Ask
	if in.AgentID != "" {
		d = core.Block
	}
	what := fmt.Sprintf("git %s takes %d file(s), %d of them edited by another live session or leaf: %s%s", verb,
		max(n.total, n.claimed), n.claimed, strings.Join(taken, "; "), more)
	hint := "stage only your hunks: " + ownHunks + "; or leave the file and tell its owner"
	if verb == "commit" {
		hint = "commit only your own files: git commit -- <your files> (a bare commit takes everything staged)"
	}
	return core.Result{Decision: d, Reason: core.SafetyMessage(in, d, what, "", hint)}
}

// interactive reports hunk mode: add -i/-p or commit -p (commit -i is --include, not interactive).
func interactive(verb string, flags []string) bool {
	return slices.ContainsFunc(flags, func(f string) bool {
		if f == "-p" || f == "--patch" {
			return true
		}
		return verb == "add" && (f == "-i" || f == "--interactive")
	})
}

// stagesNothing reports a dry run (add -n, --dry-run) or add -N, which records intent-to-add and stages no content.
func stagesNothing(verb string, flags []string) bool {
	dry, intent := false, false
	for _, f := range flags { // the last of a flag and its --no- form wins, as in git
		switch {
		case f == "--dry-run" || verb == "add" && clusterHas(f, 'n'):
			dry = true
		case f == "--no-dry-run":
			dry = false
		case verb == "add" && (f == "--intent-to-add" || clusterHas(f, 'N')):
			intent = true
		case verb == "add" && f == "--no-intent-to-add":
			intent = false
		}
	}
	return dry || intent
}

// includesStaged reports commit --include (-i), which commits the index along with the operands.
func includesStaged(verb string, flags []string) bool {
	return verb == "commit" && slices.ContainsFunc(flags, func(f string) bool {
		return f == "--include" || clusterHas(f, 'i')
	})
}

// stageScopes are the absolute paths (files or directories) a git add/commit stages; top means the whole tree.
func stageScopes(ctx context.Context, top, target, verb string, args []string) ([]string, error) {
	w := splitStage(verb, args)
	fromFile, err := readPathspecFile(target, w)
	if err != nil {
		return nil, err
	}
	ops := append(w.ops, fromFile...)
	if len(ops) > 0 {
		scopes, err := pathScopes(ctx, top, target, ops)
		if err != nil || !includesStaged(verb, w.flags) {
			return scopes, err
		}
		staged, err := gitPaths(ctx, top, top, "diff", "--cached", "--name-only", "-z")
		return append(scopes, staged...), err
	}
	if file, _ := pathspecFile(w); file != "" && verb == "add" {
		return nil, nil // add --pathspec-from-file stages only the file's entries; an empty file stages nothing
	}
	if w.stagesAll(verb) || verb == "add" {
		return []string{top}, nil // add -A/-u with no pathspec, commit -a
	}
	return gitPaths(ctx, top, top, "diff", "--cached", "--name-only", "-z")
}

// pathScopes are the files (tracked, untracked or staged-deleted) matching pathspecs ops.
func pathScopes(ctx context.Context, top, target string, ops []string) ([]string, error) {
	args := append([]string{"ls-files", "--cached", "--others", "--exclude-standard", "--full-name", "-z", "--"}, ops...)
	files, err := gitPaths(ctx, top, target, args...)
	if err != nil {
		return nil, err
	}
	// Staged deletions are no longer in the index but still belong to the command's pathspec.
	args = append([]string{"diff", "--cached", "--name-only", "-z", "--"}, ops...)
	staged, err := gitPaths(ctx, top, target, args...)
	return append(files, staged...), err
}

// readPathspecFile reads the elements of --pathspec-from-file: LF-separated and C-quoted, or NUL-separated with -z.
func readPathspecFile(dir string, w stageWords) ([]string, error) {
	file, nul := pathspecFile(w)
	if file == "" {
		return nil, nil
	}
	if file == "-" {
		return nil, errors.New("--pathspec-from-file=- reads stdin, which the hook cannot see")
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	sep := "\n"
	if nul {
		sep = "\x00"
	}
	var out []string
	for line := range strings.SplitSeq(string(raw), sep) {
		if !nul {
			line = unquotePathspec(strings.TrimSuffix(line, "\r"))
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// unquotePathspec undoes git's C-style quoting of one pathspec line; a line that does not parse is kept as-is.
func unquotePathspec(line string) string {
	if !strings.HasPrefix(line, `"`) {
		return line
	}
	if s, err := strconv.Unquote(line); err == nil {
		return s
	}
	return line
}

// pathspecFile is the --pathspec-from-file operand (`F` or `=F` form) and whether --pathspec-file-nul is set.
func pathspecFile(w stageWords) (file string, nul bool) {
	file = w.values["--pathspec-from-file"]
	for _, f := range w.flags {
		if v, ok := strings.CutPrefix(f, "--pathspec-from-file="); ok {
			file = v
		}
	}
	return file, slices.Contains(w.flags, "--pathspec-file-nul")
}

// gitPaths reads NUL-delimited repo-relative names without Git's display quoting or whitespace trimming.
func gitPaths(ctx context.Context, top, dir string, args ...string) ([]string, error) {
	raw, err := proc.RunGit(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for s := range strings.SplitSeq(raw, "\x00") {
		if s != "" {
			out = append(out, filepath.Join(top, s))
		}
	}
	return out, nil
}

// commitValueFlags are git commit options whose value is the next word.
var commitValueFlags = map[string]bool{"-m": true, "-F": true, "-C": true, "-c": true, "--author": true,
	"--date": true, "-t": true, "--fixup": true, "--squash": true, "--message": true, "--file": true,
	"--template": true, "--reuse-message": true, "--reedit-message": true,
	"--trailer": true, "--cleanup": true, "--pathspec-from-file": true}

// allTracked are the add flags that stage every tracked change when no pathspec is given (commit: -a/--all only).
var allTracked = map[string]bool{"-A": true, "--all": true, "-u": true, "--update": true}

// stageWords is an add/commit's words by role: pathspecs, option flags, and the value each value-taking flag took.
type stageWords struct {
	ops    []string
	flags  []string
	values map[string]string
}

// splitStage sorts an add/commit's words; `--` ends the options and a flag's value is never read as a flag.
func splitStage(verb string, args []string) stageWords {
	w := stageWords{values: map[string]string{}}
	words, tail := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		words, tail = args[:i], args[i+1:]
	}
	for i := 0; i < len(words); i++ {
		a := words[i]
		if !strings.HasPrefix(a, "-") {
			w.ops = append(w.ops, a)
			continue
		}
		w.flags = append(w.flags, a)
		if takesValue, _ := stageFlag(verb, a); takesValue && i+1 < len(words) {
			w.values[a] = words[i+1]
			i++
		}
	}
	w.ops = append(w.ops, tail...)
	return w
}

// stagesAll reports a flag that stages every tracked change when no pathspec is given.
func (w stageWords) stagesAll(verb string) bool {
	return slices.ContainsFunc(w.flags, func(f string) bool {
		_, all := stageFlag(verb, f)
		return all
	})
}

// trackedOnly reports commit -a or add -u without -A: the verbs that never stage an untracked file.
func (w stageWords) trackedOnly(verb string) bool {
	has := func(flags ...string) bool {
		return slices.ContainsFunc(w.flags, func(f string) bool { return slices.Contains(flags, f) })
	}
	if verb == "commit" {
		return w.stagesAll(verb)
	}
	return has("-u", "--update") && !has("-A", "--all")
}

// tracked is dirty without the untracked files.
func tracked(ctx context.Context, top string, dirty map[string]bool) (map[string]bool, error) {
	untracked, err := gitPaths(ctx, top, top, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	out := maps.Clone(dirty)
	for _, p := range untracked {
		delete(out, p)
	}
	return out, nil
}

// stageFlag classifies one add/commit flag: whether the next word is its value, and whether it stages all tracked.
func stageFlag(verb, a string) (takesValue, stagesAll bool) {
	if verb != "commit" {
		return a == "--pathspec-from-file", allTracked[a]
	}
	return commitValueFlags[a] || valueCluster(a), a == "--all" || clusterHas(a, 'a')
}

// clusterStops are the short git commit letters that take a value, attached (-u/-S) or from the next word.
const clusterStops = "uSmFCct"

// shortCluster is the letters of a short-flag cluster such as -am; "" for a long flag, a lone dash or a word.
func shortCluster(a string) string {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return ""
	}
	return a[1:]
}

// valueCluster is a cluster such as -am whose last letter takes the next word; -mc or -um attach it instead.
func valueCluster(a string) bool {
	c := shortCluster(a)
	return len(c) > 1 && strings.IndexAny(c, clusterStops) == len(c)-1 && strings.IndexByte("mFCct", c[len(c)-1]) >= 0
}

// clusterHas reports a short-flag cluster holding letter before any value-taking letter.
func clusterHas(a string, letter byte) bool {
	c := shortCluster(a)
	i, stop := strings.IndexByte(c, letter), strings.IndexAny(c, clusterStops)
	return i >= 0 && (stop < 0 || i < stop)
}

// underAny reports whether p equals or sits under one of scopes.
func underAny(p string, scopes []string) bool {
	return slices.ContainsFunc(scopes, func(s string) bool {
		return paths.Within(p, s)
	})
}
