package wthook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/clean"
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

// NestedConfigSweep runs on SessionStart: removes the nested settings.local.json; a failure is a stderr note only.
func NestedConfigSweep(*core.Input) core.Result {
	if _, err := clean.SweepNestedLocal(); err != nil {
		return core.Result{Stderr: "nested-config-sweep: " + err.Error()}
	}
	return core.Result{}
}

// Busy runs on UserPromptSubmit: the busy line when the peer set changed (mode on), then drained notices.
func Busy(in *core.Input) core.Result {
	ctx, cancel := core.HookContext("worktree-busy")
	defer cancel()
	busy := ""
	if !worktreeModeOff() {
		busy = busyLine(ctx, in)
	}
	notes, warn := noticeContext(ctx, in)
	return core.Result{Context: core.JoinContext(busy, notes), Stderr: warn}
}

// busyLine returns the busy line for the peers sharing in.CWD's checkout; "" on no peers, an error, or no change.
func busyLine(ctx context.Context, in *core.Input) string {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	top, branch, nested := CheckoutOf(ctx, cwd)
	if top == "" {
		return ""
	}
	peers, err := PeerAgents(top, nested)
	if err != nil {
		return ""
	}
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, strconv.Itoa(p.PID))
	}
	sort.Strings(ids)
	key := strings.Join(ids, ",")
	file := filepath.Join(paths.StateDir(), "worktree-busy", paths.Slug(in.SessionID, "", 80, "session"))
	if prev, _ := os.ReadFile(file); string(prev) == key {
		return ""
	}
	if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
		_ = os.WriteFile(file, []byte(key), 0o600)
	}
	if len(peers) == 0 {
		return ""
	}
	return fmt.Sprintf("Another session (%s pid %d) is working in this checkout on %s; before editing, "+
		"ask the user whether to work in a linked worktree; without a yes, keep working here.", peers[0].Comm, peers[0].PID, branch)
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
