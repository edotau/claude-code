package wthook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// Create replaces Claude Code's `.claude/worktrees/<name>` with <root>/<repo>/<name>; stdout is the path.
// Unlike other hooks it fails CLOSED: a non-zero exit is the only way to report a worktree that was not made.
func Create(in *core.Input) core.Result {
	var stderr bytes.Buffer
	// The live payload carries only `name`; the documented worktree_path/branch are honored if they ever appear.
	name := in.Nested("name")
	if name == "" {
		name = filepath.Base(filepath.Clean(in.Nested("worktree_path")))
	}
	repo, err := worktree.ResolveRepo(in.CWD)
	if err != nil {
		fmt.Fprintf(&stderr, "worktree-create: %v\n", err)
		return core.ExitResult(1, stderr.String())
	}
	dest, err := worktree.Create(repo, name, worktree.CreateOptions{
		Branch: in.Nested("branch"), Session: in.SessionID, Log: &stderr,
	})
	if err != nil {
		fmt.Fprintf(&stderr, "worktree-create: %v\n", err)
		return core.ExitResult(1, stderr.String())
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	res.Stdout = strings.TrimSpace(dest)
	return res
}

// Remove removes a hook-created worktree; no --force, so a dirty tree stays and the removal reports failure.
func Remove(in *core.Input, stderr io.Writer) int {
	path := in.Nested("worktree_path")
	if path == "" {
		fmt.Fprintln(stderr, "worktree-remove: payload has no worktree_path")
		return 1
	}
	if reason, err := worktree.InUseBy(path); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: in-use check failed: %v\n", err)
		return 1
	} else if reason != "" {
		fmt.Fprintf(stderr, "worktree-remove: %s\n", reason)
		return 1
	}
	ctx, cancel := core.HookContext("worktree-remove")
	defer cancel()
	main := paths.MainCheckout(path)
	branch, err := proc.RunGit(ctx, path, "branch", "--show-current")
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: branch probe failed, its branch is kept: %v\n", err)
	}
	head, err := proc.RunGit(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: HEAD probe failed, the ledger records no head: %v\n", err)
	}
	if _, err := proc.RunGit(ctx, main, "worktree", "remove", path); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: %v\n", err)
		return 1
	}
	branchDeleted := false
	if b := strings.TrimSpace(branch); b != "" {
		_, derr := proc.RunGit(ctx, main, "branch", "-d", b)
		branchDeleted = derr == nil
	}
	closeRemoved(main, path, strings.TrimSpace(head), branchDeleted, stderr)
	return core.ExitProceed
}

// closeRemoved records the removal in the worktree ledger; a failed write only warns.
func closeRemoved(main, path, head string, branchDeleted bool, stderr io.Writer) {
	if err := worktree.CloseLedger(main, filepath.Base(filepath.Clean(path)), worktree.LedgerClose{
		Via: "remove", Head: head, BranchDeleted: branchDeleted,
	}); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: ledger: %v\n", err)
	}
}

// worktreeModeEnv selects the mode: off|false|0 never asks; unset or anything else asks (session start, new peer).
const worktreeModeEnv = "HARNESS_WORKTREE"

// PeerAgents is the process scan; a var seam so tests never see the host's live sessions.
var PeerAgents = worktree.PeerAgents

const worktreeAskLine = "Worktree: before starting a change, ask the user whether to work in a linked worktree " +
	"(`claude-code worktree create <slug>` / EnterWorktree); without a yes, keep working in the checkout."

func worktreeModeOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(worktreeModeEnv))) {
	case "off", "false", "0":
		return true
	}
	return false
}

// Mode runs on SessionStart: the ask line on a fresh start only (not resume/clear/compact), unless the mode is off.
func Mode(in *core.Input) core.Result {
	if worktreeModeOff() || (in.Source != "" && in.Source != "startup") {
		return core.Result{}
	}
	return core.Result{Context: worktreeAskLine}
}

// Busy runs on UserPromptSubmit: the busy line when the peer set changed (mode on), then drained notices.
func Busy(in *core.Input) core.Result {
	ctx, cancel := core.HookContext("worktree-busy")
	defer cancel()
	busy, busyWarn := "", ""
	if !worktreeModeOff() {
		var err error
		busy, err = busyLine(ctx, in)
		if err != nil {
			busyWarn = fmt.Sprintf("worktree-busy: WARNING — %v", err)
		}
	}
	notes, warn := noticeContext(ctx, in)
	return core.Result{Context: core.JoinContext(busy, notes), Stderr: core.JoinContext(busyWarn, warn)}
}

// busyLine returns the peer notice and any process-scan or dedupe-state error.
func busyLine(ctx context.Context, in *core.Input) (string, error) {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	top, branch, nested := CheckoutOf(ctx, cwd)
	if top == "" {
		return "", nil
	}
	peers, err := PeersOf(ctx, top, nested)
	if err != nil {
		return "", fmt.Errorf("scan peers in %s: %w", top, err)
	}
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, strconv.Itoa(p.PID))
	}
	sort.Strings(ids)
	key := strings.Join(ids, ",")
	file := filepath.Join(paths.StateDir(), "worktree-busy", paths.Slug(in.SessionID, "", 80, "session"))
	if prev, _ := os.ReadFile(file); string(prev) == key {
		return "", nil
	}
	if err = paths.AtomicWrite(file, []byte(key), 0o600); err != nil {
		err = fmt.Errorf("write peer state %s: %w", file, err)
	}
	if len(peers) == 0 {
		return "", err
	}
	return fmt.Sprintf("Another session (%s pid %d) is working in this checkout on %s; before editing, "+
		"ask the user whether to work in a linked worktree; without a yes, keep working here.", peers[0].Comm, peers[0].PID, branch), err
}

// CheckoutOf returns cwd's toplevel, its branch, and the repo's worktrees nested inside top; "" top when not a repo.
func CheckoutOf(ctx context.Context, cwd string) (top, branch string, nested []string) {
	git := func(args ...string) string {
		out, err := proc.RunGit(ctx, cwd, args...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	top = git("rev-parse", "--show-toplevel")
	if top == "" {
		return "", "", nil
	}
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	branch = git("rev-parse", "--abbrev-ref", "HEAD")
	var others []string
	for line := range strings.SplitSeq(git("worktree", "list", "--porcelain"), "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if real, err := filepath.EvalSymlinks(path); ok && err == nil && real != top {
			others = append(others, real)
		}
	}
	return top, branch, core.NestedIn(top, others)
}

// PeersOf runs the PeerAgents scan for top under ctx's deadline; a scan past it is abandoned as an error.
func PeersOf(ctx context.Context, top string, nested []string) ([]worktree.Proc, error) {
	type scan struct {
		procs []worktree.Proc
		err   error
	}
	done := make(chan scan, 1)
	go func() {
		procs, err := PeerAgents(top, nested)
		done <- scan{procs, err}
	}()
	select {
	case s := <-done:
		return s.procs, s.err
	case <-ctx.Done():
		return nil, fmt.Errorf("process scan: %w", ctx.Err())
	}
}

// SessionLabel names a peer from Claude Code's session registry (<config>/sessions/<pid>.json; never the .key file).
func SessionLabel(p worktree.Proc) string {
	fallback := fmt.Sprintf("%s pid %d", p.Comm, p.PID)
	b, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "sessions", strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return fallback
	}
	var s struct {
		Name      string `json:"name"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(b, &s) != nil || len(s.SessionID) < 8 {
		return fallback
	}
	name := s.Name
	if name == "" {
		name = s.SessionID[:8]
	}
	return fmt.Sprintf("session %s (%s, pid %d)", name, s.SessionID[:8], p.PID)
}

// gitAnyCmd pre-screens a Bash command for any git invocation: the moment a drained notice matters most.
var gitAnyCmd = regexp.MustCompile(`\bgit\b`)

// noticeContext drains this session's inbox and adds the divergence line; warn names an infra error (fail open).
func noticeContext(ctx context.Context, in *core.Input) (text, warn string) {
	var lines []string
	for _, n := range worktree.Drain(in.SessionID) {
		lines = append(lines, fmt.Sprintf("notice (%s, from %s): %s", n.Kind, n.From, n.Text))
	}
	line, err := divergedLine(ctx, in)
	if err != nil {
		warn = fmt.Sprintf("notices: WARNING — could not check branch divergence (%v); skipped", err)
	}
	if line != "" {
		lines = append(lines, line)
	}
	return core.CapContext(strings.Join(lines, "\n")), warn
}

// divergence is a checkout whose HEAD and upstream both have commits the other lacks.
type divergence struct {
	top, head, up, branch, upName string
	ahead, behind                 int
	rewritten                     bool // some commit's patch sits on both sides
}

// divergedLine names a shared checkout's local/upstream divergence once per (HEAD, upstream) pair; "" otherwise.
func divergedLine(ctx context.Context, in *core.Input) (string, error) {
	dir := in.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	d, err := divergedAt(ctx, dir)
	if d == nil || err != nil {
		return "", err
	}
	shared, err := checkoutShared(ctx, dir)
	if !shared || err != nil {
		return "", err
	}
	if !firstSighting(in.SessionID, "diverged\t"+d.head+"\t"+d.up) {
		return "", nil
	}
	why := "both sides have new commits; merge " + d.upName + " before committing on top"
	if d.rewritten {
		why = "a commit was rewritten; do not commit until it is reconciled"
	}
	return fmt.Sprintf("local %s diverged from %s (ahead %d, behind %d): %s%s", d.branch, d.upName, d.ahead, d.behind,
		why, d.authors(ctx, in)), nil
}

// divergedAt reads dir's ahead/behind counts against its upstream; nil when there is no upstream or no divergence.
func divergedAt(ctx context.Context, dir string) (*divergence, error) {
	counts, ok, err := core.Git(ctx, dir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil || !ok {
		return nil, err // !ok: not a repo, detached, or no upstream
	}
	a, b := bothSides(counts[0])
	if a == 0 || b == 0 {
		return nil, nil
	}
	ids, _, err := core.Git(ctx, dir, "rev-parse", "--show-toplevel", "HEAD", "@{upstream}", "--abbrev-ref", "HEAD",
		"@{upstream}")
	if len(ids) != 5 {
		return nil, err
	}
	// A rewrite (amend, rebase) leaves the same patch on both sides; --cherry-mark tags those commits "=".
	marked, _, err := core.Git(ctx, dir, "rev-list", "--left-right", "--cherry-mark", "HEAD...@{upstream}")
	if err != nil {
		return nil, err
	}
	rewritten := slices.ContainsFunc(marked, func(l string) bool { return strings.HasPrefix(l, "=") && hasPatch(ctx, ids[0], l[1:]) })
	return &divergence{top: ids[0], head: ids[1], up: ids[2], branch: ids[3], upName: ids[4], ahead: a, behind: b,
		rewritten: rewritten}, nil
}

// hasPatch reports whether commit changes a file; empty commits share one patch-id yet replace nothing.
func hasPatch(ctx context.Context, dir, commit string) bool {
	files, _, err := core.Git(ctx, dir, "diff-tree", "--root", "-r", "--name-only", "--no-commit-id", commit)
	return err != nil || len(files) > 0 && files[0] != ""
}

// bothSides parses rev-list --left-right --count output; zeros for anything else.
func bothSides(line string) (ahead, behind int) {
	f := strings.Fields(line)
	if len(f) != 2 {
		return 0, 0
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind
}

// checkoutShared is true for the live config tree or a checkout a peer session works in.
func checkoutShared(ctx context.Context, dir string) (bool, error) {
	top, _, nested := CheckoutOf(ctx, dir)
	if top == "" {
		return false, nil
	}
	if top == core.CanonPath(paths.ConfigDir()) {
		return true, nil
	}
	peers, err := PeersOf(ctx, top, nested)
	return len(peers) > 0, err
}

// authors names, from the claims ledger, whose commits sit on each side; "" when the ledger knows none.
func (d *divergence) authors(ctx context.Context, in *core.Input) string {
	byCommit := map[string]string{}
	self := paths.Slug(in.SessionID, "", 80, "session")
	_ = worktree.ScanClaims(claimAlive, func(pid int, slug string, c worktree.Claim) {
		if c.Commit != "" {
			byCommit[c.Commit] = worktree.Label(pid, slug, c.Agent)
			if slug == self {
				byCommit[c.Commit] = "this session"
			}
		}
	})
	if len(byCommit) == 0 {
		return ""
	}
	var parts []string
	for _, side := range [][2]string{{"ahead", d.up + ".." + d.head}, {"behind", d.head + ".." + d.up}} {
		if who := committers(ctx, d.top, side[1], byCommit); len(who) > 0 {
			parts = append(parts, side[0]+": "+strings.Join(who, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// committers maps the commits in rng (at most 50) to the claimants whose short shas prefix them.
func committers(ctx context.Context, top, rng string, byCommit map[string]string) []string {
	shas, ok, err := core.Git(ctx, top, "rev-list", "--max-count=50", rng)
	if err != nil || !ok {
		return nil
	}
	var who []string
	for _, sha := range shas {
		for short, label := range byCommit {
			if len(short) >= 7 && strings.HasPrefix(sha, short) && !slices.Contains(who, label) {
				who = append(who, label)
			}
		}
	}
	slices.Sort(who)
	return who
}
