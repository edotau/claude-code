package wthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
var gitStageCmd = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bgit\b[^;&|\n]*\b(?:add|commit)\b`)
})

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

// leafEnded: the leaf's last assistant turn ended its turn, or handed back and the hand-back's result is not a denial.
func leafEnded(transcript string) bool {
	lines, ok := tailLines(transcript)
	if !ok {
		return false
	}
	failed := map[string]bool{} // tool_use_id → is_error, for results after the last assistant turn
	for _, line := range slices.Backward(lines) {
		var rec leafRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Type == "assistant" {
			return turnEnded(rec, failed)
		}
		for _, c := range rec.Message.Content {
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

// turnEnded: an assistant turn that ended, or a SubagentHandback whose later result is not a denial.
func turnEnded(rec leafRecord, failed map[string]bool) bool {
	return rec.Message.StopReason == "end_turn" || slices.ContainsFunc(rec.Message.Content, func(c leafContent) bool {
		isErr, seen := failed[c.ID]
		return c.Type == "tool_use" && c.Name == "SubagentHandback" && seen && !isErr
	})
}

// leafRecord is the slice of a transcript line leafEnded reads; a denied tool call's result carries is_error.
type leafRecord struct {
	Type    string `json:"type"`
	Message struct {
		StopReason string        `json:"stop_reason"`
		Content    []leafContent `json:"content"`
	} `json:"message"`
}

type leafContent struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
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

// claimView is one check's inputs: the hook payload, its deadline and the live claims.
type claimView struct {
	ctx    context.Context
	in     *core.Input
	claims map[string][]claimHit
}

// ClaimCheck notes a dirty claimed file before an edit; asks (leaf: blocks) when git add/commit stages one.
// Any git command also drains this session's notices and the divergence line into its context.
func ClaimCheck(in *core.Input) core.Result {
	cmd := in.ToolCommand()
	if in.SessionID == "" || in.ToolName == "Bash" && !gitAnyCmd().MatchString(cmd) {
		return core.Result{}
	}
	ctx, cancel := core.HookContext("worktree-claim-check")
	defer cancel()
	if in.ToolName != "Bash" {
		return claimResult(ctx, in)
	}
	notes, warn := noticeContext(ctx, in, newRun(ctx, in))
	res := core.Result{}
	if gitStageCmd().MatchString(cmd) {
		res = claimResult(ctx, in)
	}
	res.Context = core.JoinContext(notes, res.Context)
	res.Stderr = strings.TrimSpace(warn + "\n" + res.Stderr)
	return res
}

// claimResult runs the edit or stage check against the live claims.
func claimResult(ctx context.Context, in *core.Input) core.Result {
	claims, err := liveClaims(in)
	if err != nil {
		return claimWarn(err)
	}
	if len(claims) == 0 {
		return core.Result{}
	}
	v := claimView{ctx: ctx, in: in, claims: claims}
	if in.ToolName == "Bash" {
		return v.stageCheck()
	}
	return v.editCheck()
}

// claimWarn is the fail-open result for a check that could not run.
func claimWarn(err error) core.Result {
	return core.Result{Stderr: fmt.Sprintf("worktree-claim-check: WARNING — could not check file claims (%v); allowed", err)}
}

// editPath is the Write/Edit target, absolute and canonical; "" when the payload has none.
func editPath(in *core.Input) string {
	p := in.Nested("tool_input", "file_path")
	if p == "" {
		return ""
	}
	return core.CanonPath(core.AnchorPath(p, in.CWD))
}

// editCheck returns the once-per-(session, path, who) advisory for an edit of a dirty claimed file.
func (v claimView) editCheck() core.Result {
	p := editPath(v.in)
	if len(v.claims[p]) == 0 {
		return core.Result{}
	}
	top, dirty, err := dirtySet(v.ctx, filepath.Dir(p))
	if err != nil {
		return claimWarn(err)
	}
	hits, err := v.freshHits(top, p, dirty)
	if err != nil {
		return claimWarn(err)
	}
	var lines []string
	for _, h := range hits {
		if firstSighting(v.in.SessionID, p+"\t"+h.Who) {
			lines = append(lines, fmt.Sprintf("%s has uncommitted edits by %s (since %s); edit only your own hunks "+
				"and commit them with `git add -p`, or hand the change to its owner.", relTo(top, p), h.Who, clock(h.TS)))
		}
	}
	return core.Result{Context: strings.Join(lines, "\n")}
}

// dirtySet is dir's repo toplevel and its dirty paths (absolute, untracked included); "" top outside a repo.
func dirtySet(ctx context.Context, dir string) (string, map[string]bool, error) {
	top, _ := paths.CheckoutOf(dir)
	if top == "" {
		return "", nil, nil
	}
	top = core.CanonPath(top)
	raw, err := proc.RunGit(ctx, top, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return "", nil, fmt.Errorf("git status in %s: %w", top, err)
	}
	dirty := map[string]bool{}
	for _, e := range proc.StatusEntries(raw) {
		dirty[filepath.Join(top, e.Path)] = true
	}
	return top, dirty, nil
}

// freshHits keeps p's claims when p is dirty and no claim predates p's last commit (an older claim landed).
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
	return slices.DeleteFunc(slices.Clone(v.claims[p]), func(h claimHit) bool { return h.TS < landed }), nil
}

// claimsSeenWait bounds the claims-seen lock wait; PreToolUse must not stall, so a held lock fails open.
const claimsSeenWait = time.Second

// firstSighting records key in this session's claims-seen file; false when it was already shown.
func firstSighting(session, key string) bool {
	file := filepath.Join(paths.StateDir(), "hooks", "claims-seen", sessionSlug(session))
	first := true
	err := os.MkdirAll(filepath.Dir(file), 0o700)
	if err == nil { // the seen file doubles as its own lock: no stray .lock sibling
		var ran bool
		ran, err = paths.TryFileLock(file, claimsSeenWait, func() error {
			var rerr error
			first, rerr = appendIfNew(file, key)
			return rerr
		})
		if err == nil && !ran {
			err = errors.New("claims-seen lock busy")
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "worktree-claim-check: claims-seen: %v\n", err)
	}
	return first
}

// appendIfNew appends key to file unless a line already holds it; false when it was already there.
func appendIfNew(file, key string) (bool, error) {
	prev, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	if slices.Contains(strings.Split(string(prev), "\n"), key) {
		return false, nil
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return true, err
	}
	_, werr := f.WriteString(key + "\n")
	return true, errors.Join(werr, f.Close())
}

func relTo(top, p string) string {
	if rel, err := filepath.Rel(top, p); err == nil {
		return rel
	}
	return p
}

func clock(ts int64) string { return time.Unix(ts, 0).Format("15:04") }

// stageCheck asks (a leaf: blocks) when a git add/commit in the command would stage a dirty claimed file.
func (v claimView) stageCheck() core.Result {
	var res core.Result
	shell.EachGit(v.in.ToolCommand(), v.in.CWD, func(target, verb string, args []string) bool {
		res = v.stageVerdict(target, verb, args)
		if verb == shell.Uninspectable {
			res = v.uninspectableVerdict()
		}
		return res.Decision != core.Allow || res.Stderr != ""
	})
	return res
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
	if verb != "add" && verb != "commit" || interactive(verb, splitStage(verb, args).flags) {
		return core.Result{}
	}
	top, dirty, err := dirtySet(v.ctx, target)
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
		for _, h := range hits {
			taken = append(taken, fmt.Sprintf("%s takes %s's uncommitted hunks (since %s)", relTo(top, p), h.Who,
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
	hint := "git add -p <path> for your hunks only, or leave the file and tell its owner"
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
