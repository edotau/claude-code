package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/shell"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/notices"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// claimAlive probes a claims file's owner pid; a var seam so tests decide which owners are live.
var claimAlive = proc.Alive

// claimShown caps how many claimed paths one ask names.
const claimShown = 5

// gitStageCmd pre-screens a Bash command for a git add/commit before any claims file is read.
var gitStageCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\b(?:add|commit)\b`)

// claimHit is one live claim on a path by someone other than the caller: who, and their newest edit's time.
type claimHit struct {
	Who string
	TS  int64
}

// liveClaims maps each path other live parties claim to its claimants; a dead owner pid's file is removed.
func liveClaims(in *core.Input) (map[string][]claimHit, error) {
	self := paths.Slug(in.SessionID, "", 80, "session")
	leaf := siblingLeaf(in)
	labels := map[int]string{}
	byPath := map[string]map[string]int64{}
	err := notices.ScanClaims(claimAlive, func(pid int, slug string, c notices.Claim) {
		if c.Path == "" {
			return
		}
		w := leaf(c) // own session: only a sibling leaf's claim counts
		if slug != self {
			if labels[pid] == "" {
				labels[pid] = sessionLabel(worktree.Proc{PID: pid, Comm: "claude"})
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
func siblingLeaf(in *core.Input) func(notices.Claim) string {
	live := map[string]bool{}
	return func(c notices.Claim) string {
		if c.Agent == "" || c.Agent == in.AgentID {
			return ""
		}
		ok, seen := live[c.Agent]
		if !seen {
			st, err := os.Stat(filepath.Join(inflight.SubagentDir(in.TranscriptPath), "agent-"+c.Agent+".jsonl"))
			ok = err == nil && time.Since(st.ModTime()) < inflight.Idle
			live[c.Agent] = ok
		}
		if !ok {
			return ""
		}
		return "leaf " + c.Agent[:min(8, len(c.Agent))]
	}
}

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

// worktreeClaimCheck notes a dirty claimed file before an edit; asks (leaf: blocks) when git add/commit stages one.
// Any git command also drains this session's notices and the divergence line into its context.
func worktreeClaimCheck(in *core.Input) core.Result {
	cmd := in.ToolCommand()
	if in.SessionID == "" || in.ToolName == "Bash" && !gitAnyCmd.MatchString(cmd) {
		return core.Result{}
	}
	ctx, cancel := core.HookContext("worktree-claim-check")
	defer cancel()
	if in.ToolName != "Bash" {
		return claimResult(ctx, in)
	}
	notes, warn := noticeContext(ctx, in)
	res := core.Result{}
	if gitStageCmd.MatchString(cmd) {
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
	if !filepath.IsAbs(p) {
		p = filepath.Join(in.CWD, p)
	}
	return canonPath(p)
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
	out, ok, err := gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || !ok {
		return "", nil, err
	}
	top := canonPath(out[0])
	raw, err := exec.CommandContext(ctx, "git", "-C", top, "status", "--porcelain", "-z", "--untracked-files=all").Output()
	if err != nil {
		return "", nil, fmt.Errorf("git status in %s: %w", top, err)
	}
	dirty := map[string]bool{}
	recs := strings.Split(string(raw), "\x00")
	for i := 0; i < len(recs); i++ {
		if r := recs[i]; len(r) > 3 {
			dirty[filepath.Join(top, r[3:])] = true
			if r[0] == 'R' || r[0] == 'C' {
				i++ // a rename's second record is its source path
			}
		}
	}
	return top, dirty, nil
}

// freshHits keeps p's claims when p is dirty and no claim predates p's last commit (an older claim landed).
func (v claimView) freshHits(top, p string, dirty map[string]bool) ([]claimHit, error) {
	if top == "" || !dirty[p] {
		return nil, nil
	}
	out, _, err := gitOut(v.ctx, top, "log", "-1", "--format=%ct", "--", p)
	if err != nil {
		return nil, err
	}
	var landed int64
	if len(out) > 0 {
		landed, _ = strconv.ParseInt(out[0], 10, 64)
	}
	return slices.DeleteFunc(slices.Clone(v.claims[p]), func(h claimHit) bool { return h.TS < landed }), nil
}

// firstSighting records key in this session's claims-seen file; false when it was already shown.
func firstSighting(session, key string) bool {
	file := filepath.Join(paths.StateDir(), "hooks", "claims-seen", paths.Slug(session, "", 80, "session"))
	first := true
	err := os.MkdirAll(filepath.Dir(file), 0o700)
	if err == nil {
		err = paths.WithFileLock(file, func() error {
			prev, rerr := os.ReadFile(file)
			if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return rerr
			}
			if first = !slices.Contains(strings.Split(string(prev), "\n"), key); !first {
				return nil
			}
			f, oerr := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if oerr != nil {
				return oerr
			}
			_, werr := f.WriteString(key + "\n")
			return errors.Join(werr, f.Close())
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "worktree-claim-check: claims-seen: %v\n", err)
	}
	return first
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
	eachGitCall(v.in.ToolCommand(), v.in.CWD, func(target, verb string, args []string) bool {
		res = v.stageVerdict(target, verb, args)
		return res.Decision == core.Allow && res.Stderr == ""
	})
	return res
}

// eachGitCall calls fn with each git invocation in cmd, following cd, until fn returns false.
func eachGitCall(cmd, cwd string, fn func(target, verb string, args []string) bool) {
	for _, seg := range shell.Segments(cmd) {
		toks := shell.Tokens(seg)
		if next, ok := cdTarget(toks, cwd); ok {
			cwd = next
			continue
		}
		for i, t := range toks {
			if t == "git" && !fn(shell.GitInvocation(toks[i+1:], cwd)) {
				return
			}
		}
	}
}

// stageVerdict checks one git invocation's staged set against the claims.
func (v claimView) stageVerdict(target, verb string, args []string) core.Result {
	if verb != "add" && verb != "commit" || slices.ContainsFunc(args, isPatchFlag) {
		return core.Result{}
	}
	top, dirty, err := dirtySet(v.ctx, target)
	if top == "" || err != nil {
		return warnIf(err)
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

// warnIf is claimWarn for a non-nil err, else no result.
func warnIf(err error) core.Result {
	if err != nil {
		return claimWarn(err)
	}
	return core.Result{}
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
	return core.Result{Decision: d, Reason: safetyMessage(in, d, what, "", hint)}
}

func isPatchFlag(a string) bool {
	return a == "-p" || a == "--patch" || a == "-i" || a == "--interactive"
}

// stageScopes are the absolute paths (files or directories) a git add/commit stages; top means the whole tree.
func stageScopes(ctx context.Context, top, target, verb string, args []string) ([]string, error) {
	ops, all := stageOperands(verb, args)
	if len(ops) > 0 {
		out := make([]string, 0, len(ops))
		for _, o := range ops {
			out = append(out, canonPath(filepath.Join(target, o)))
		}
		return out, nil
	}
	if all || verb == "add" {
		return []string{top}, nil // add -A/-u with no pathspec, commit -a
	}
	staged, _, err := gitOut(ctx, top, "diff", "--cached", "--name-only")
	var out []string
	for _, s := range staged {
		if s != "" {
			out = append(out, filepath.Join(top, s))
		}
	}
	return out, err
}

// commitValueFlags are git commit options whose value is the next word.
var commitValueFlags = map[string]bool{"-m": true, "-F": true, "-C": true, "-c": true, "--author": true,
	"--date": true, "-t": true, "--fixup": true, "--squash": true, "--message": true, "--file": true,
	"--template": true, "--reuse-message": true, "--reedit-message": true}

// allTracked are the add flags that stage every tracked change when no pathspec is given (commit: -a/--all only).
var allTracked = map[string]bool{"-A": true, "--all": true, "-u": true, "--update": true}

// stageOperands splits a git add/commit's words into pathspecs and whether an all-tracked flag is present.
func stageOperands(verb string, args []string) (ops []string, all bool) {
	words, tail := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		words, tail = args[:i], args[i+1:]
	}
	for i := 0; i < len(words); i++ {
		a := words[i]
		if !strings.HasPrefix(a, "-") {
			ops = append(ops, a)
			continue
		}
		takesValue, stagesAll := stageFlag(verb, a)
		all = all || stagesAll
		if takesValue {
			i++
		}
	}
	return append(ops, tail...), all
}

// stageFlag classifies one add/commit flag: whether the next word is its value, and whether it stages all tracked.
func stageFlag(verb, a string) (takesValue, stagesAll bool) {
	if verb != "commit" {
		return false, allTracked[a]
	}
	return commitValueFlags[a] || valueCluster(a), a == "--all" || isShortCluster(a, 'a')
}

// valueCluster is a short-flag cluster such as -am whose last flag takes the next word as its value.
func valueCluster(a string) bool {
	return len(a) > 2 && !strings.HasPrefix(a, "--") && strings.ContainsRune("mFCct", rune(a[len(a)-1]))
}

// isShortCluster reports a short-flag cluster such as -am that holds flag.
func isShortCluster(a string, flag rune) bool {
	return !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], flag)
}

// underAny reports whether p equals or sits under one of scopes.
func underAny(p string, scopes []string) bool {
	return slices.ContainsFunc(scopes, func(s string) bool {
		return p == s || strings.HasPrefix(p, s+string(filepath.Separator))
	})
}
