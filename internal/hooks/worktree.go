package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/clean"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// worktreeCreate replaces Claude Code's `.claude/worktrees/<name>` with <root>/<repo>/<name>; stdout is the path.
// Unlike other hooks it fails CLOSED: a non-zero exit is the only way to report a worktree that was not made.
func worktreeCreate(in *core.Input) core.Result {
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

// worktreeRemove removes a hook-created worktree; no --force, so a dirty tree stays and the removal reports failure.
func worktreeRemove(in *core.Input, stderr io.Writer) int {
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
	branch, err := exec.CommandContext(ctx, "git", "-C", path, "branch", "--show-current").Output()
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: branch probe failed, its branch is kept: %v\n", err)
	}
	head, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD").Output()
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: HEAD probe failed, the ledger records no head: %v\n", err)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", main, "worktree", "remove", path).CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: %v: %s\n", err, strings.TrimSpace(string(out)))
		return 1
	}
	branchDeleted := false
	if b := strings.TrimSpace(string(branch)); b != "" {
		branchDeleted = exec.CommandContext(ctx, "git", "-C", main, "branch", "-d", b).Run() == nil
	}
	if err := worktree.CloseLedger(main, filepath.Base(filepath.Clean(path)), worktree.LedgerClose{
		Via: "remove", Head: strings.TrimSpace(string(head)), BranchDeleted: branchDeleted,
	}); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: ledger: %v\n", err)
	}
	return core.ExitProceed
}

// worktreeModeEnv selects the mode: off|false|0 is silent; on|true|1 directs a worktree; unset or anything else asks.
const worktreeModeEnv = "HARNESS_WORKTREE"

// peerAgents is the process scan; a var seam so tests never see the host's live sessions.
var peerAgents = worktree.PeerAgents

const worktreeAskLine = "Worktree: before starting a change, ask the user whether to work in a linked worktree " +
	"(`claude-code worktree create <slug>` / EnterWorktree); without a yes, keep working in the checkout."

const worktreeOnLine = "Worktree (HARNESS_WORKTREE=on): unless this session is already in a linked worktree, " +
	"create one before the first edit (`claude-code worktree create <slug>` / EnterWorktree) without asking; " +
	"merge it back when done (`claude-code worktree merge <slug>`)."

func worktreeModeOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(worktreeModeEnv))) {
	case "off", "false", "0":
		return true
	}
	return false
}

func worktreeModeOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(worktreeModeEnv))) {
	case "on", "true", "1":
		return true
	}
	return false
}

// worktreeStartLine is the SessionStart line for the mode: the directive when on, else the ask.
func worktreeStartLine() string {
	if worktreeModeOn() {
		return worktreeOnLine
	}
	return worktreeAskLine
}

// worktreeMode runs on SessionStart: the mode's line on a fresh start only (not resume/clear/compact), unless off.
func worktreeMode(in *core.Input) core.Result {
	if worktreeModeOff() || (in.Source != "" && in.Source != "startup") {
		return core.Result{}
	}
	return core.Result{Context: worktreeStartLine()}
}

// nestedConfigSweep runs on SessionStart: removes the nested settings.local.json; a failure is a stderr note only.
func nestedConfigSweep(*core.Input) core.Result {
	if _, err := clean.SweepNestedLocal(); err != nil {
		return core.Result{Stderr: "nested-config-sweep: " + err.Error()}
	}
	return core.Result{}
}

// worktreeBusy runs on UserPromptSubmit: the busy line when the peer set changed (mode on), then drained notices.
func worktreeBusy(in *core.Input) core.Result {
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
	top, branch, nested := checkoutOf(ctx, cwd)
	if top == "" {
		return ""
	}
	peers, err := peerAgents(top, nested)
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
	then := "ask the user whether to work in a linked worktree; without a yes, keep working here."
	if worktreeModeOn() {
		then = "move into a linked worktree without asking (HARNESS_WORKTREE=on)."
	}
	return fmt.Sprintf("Another session (%s pid %d) is working in this checkout on %s; before editing, %s",
		peers[0].Comm, peers[0].PID, branch, then)
}

// checkoutOf returns cwd's toplevel, its branch, and the repo's worktrees nested inside top; "" top when not a repo.
func checkoutOf(ctx context.Context, cwd string) (top, branch string, nested []string) {
	git := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
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
	return top, branch, nestedIn(top, others)
}
