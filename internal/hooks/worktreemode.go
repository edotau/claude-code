// `worktree-mode` (SessionStart) and `worktree-busy` (UserPromptSubmit): tell Claude when to ask the user about a
// linked worktree. They never create one. HARNESS_WORKTREE=off|false|0 stays silent; any other value (or unset) asks
// once at session start, and again when a peer session joins the checkout.
package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// worktreeModeEnv selects the mode: off|false|0 never asks; unset or anything else asks (session start, new peer).
const worktreeModeEnv = "HARNESS_WORKTREE"

// peerAgents is the process scan; a var seam so tests never see the host's live sessions.
var peerAgents = worktree.PeerAgents

const worktreeAskLine = "Worktree: before starting a change, ask the user whether to work in a linked worktree " +
	"(`claude-code worktree create <slug>` / EnterWorktree); without a yes, keep working in the checkout."

func worktreeModeOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(worktreeModeEnv))) {
	case "off", "false", "0":
		return true
	}
	return false
}

// worktreeMode runs on SessionStart: the ask line once, unless the mode is off.
func worktreeMode(in *Input) Result {
	if worktreeModeOff() {
		return Result{}
	}
	return Result{Context: worktreeAskLine}
}

// worktreeBusy runs on UserPromptSubmit: the busy line only when the peer set changed since last report.
func worktreeBusy(in *Input) Result {
	if worktreeModeOff() {
		return Result{}
	}
	return Result{Context: busyLine(in)}
}

// busyLine returns the busy line for the peers sharing in.CWD's checkout; "" on no peers, an error, or no change.
func busyLine(in *Input) string {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ctx, cancel := hookContext("worktree-busy")
	defer cancel()
	top, branch, linked := checkoutOf(ctx, cwd)
	if top == "" {
		return ""
	}
	peers, err := peerAgents(top, linked)
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

// checkoutOf returns cwd's git toplevel, its branch, and the other linked worktree paths; "" top when not a repo.
func checkoutOf(ctx context.Context, cwd string) (top, branch string, linked []string) {
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
	for line := range strings.SplitSeq(git("worktree", "list", "--porcelain"), "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if real, err := filepath.EvalSymlinks(path); ok && err == nil && real != top {
			linked = append(linked, real)
		}
	}
	return top, branch, linked
}
