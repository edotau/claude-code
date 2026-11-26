// Package lane lands a finished leaf's linked worktree and queues the ones it cannot land.
package lane

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/quality"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

// laneLandArg starts the detached worker (laneland_worker.go): `leaf <lane> <session> <transcript>`.
const laneLandArg = "--internal-lane-land"

// A private detached entry, like the bench worker's: it outlives the hook's budget.
func init() {
	switch {
	case len(os.Args) == 6 && os.Args[1] == laneLandArg && os.Args[2] == "leaf":
		runLeafLane(os.Args[3], os.Args[4], os.Args[5])
		os.Exit(0)
	case len(os.Args) == 3 && os.Args[1] == agent.LaneLandJobArg:
		runJobLane(os.Args[2])
		os.Exit(0)
	}
}

// laneSpawn starts the detached worker outside any lane; a seam so tests spawn nothing.
var laneSpawn = func(args []string) bool {
	self, err := paths.SelfBinary()
	return err == nil && proc.SpawnDetached(self, args, paths.StateDir(), nil)
}

// Queue (UserPromptSubmit) lists this repo's queued lanes, each with its next command.
func Queue(in *core.Input) core.Result {
	return core.Result{Context: laneQueueContext(in.CWD, in.SessionID)}
}

// Land (SubagentStop) hands a stopped leaf's lane to the detached worker and returns at once.
func Land(in *core.Input) core.Result {
	if in.AgentID == "" || in.AgentTranscriptPath == "" {
		return core.Result{}
	}
	lane := LeafLane(in)
	if lane == "" {
		return core.Result{}
	}
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	if !laneSpawn([]string{laneLandArg, "leaf", lane, in.SessionID, in.AgentTranscriptPath}) {
		return core.Result{Stderr: "lane-land: could not start the land worker for " + lane}
	}
	return core.Result{}
}

// LeafLane is the linked worktree the leaf was spawned in (meta.json), else the one lane all its claims sit in.
func LeafLane(in *core.Input) string {
	var meta struct {
		WorktreePath string `json:"worktreePath"`
	}
	if raw, err := os.ReadFile(LeafMeta(in.AgentTranscriptPath)); err == nil && json.Unmarshal(raw, &meta) == nil && meta.WorktreePath != "" {
		return laneOf(meta.WorktreePath)
	}
	return claimedLane(in.SessionID, in.AgentID)
}

// claimedLane is the one lane the claims ledger shows agentID writing in; none or several is no guess.
func claimedLane(session, agentID string) string {
	file := filepath.Join(worktree.ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", inflight.SessionOwner(), worktree.SessionSlug(session)))
	var lanes []string
	for line := range bytes.SplitSeq(quality.TailOf(file), []byte("\n")) {
		var c worktree.Claim
		if json.Unmarshal(line, &c) != nil || c.Agent != agentID || c.Path == "" {
			continue
		}
		if l := laneOf(c.Path); l != "" && !slices.Contains(lanes, l) {
			lanes = append(lanes, l)
		}
	}
	if len(lanes) != 1 {
		return ""
	}
	return lanes[0]
}

func LeafMeta(transcript string) string {
	return strings.TrimSuffix(transcript, ".jsonl") + ".meta.json"
}

// laneOf is p's checkout when it is a linked worktree under its repo's lane root (or the dotted legacy root).
func laneOf(p string) string {
	top, main := paths.CheckoutOf(p)
	if top == "" || paths.SamePath(top, main) {
		return ""
	}
	root := paths.LaneRoot(main)
	legacy := filepath.Join(filepath.Dir(root), filepath.Base(filepath.Clean(main)))
	if !paths.InsideDir(top, root) && !paths.InsideDir(top, legacy) {
		return ""
	}
	return paths.Canon(top)
}

// laneVerdict is "none" for a clean UNFIXED line, "missing" without one, else the unfixed items.
func laneVerdict(report string) string {
	items := quality.UnfixedItems(report)
	switch {
	case len(items) == 0:
		return "missing"
	case quality.UnfixedNone(items):
		return "none"
	}
	return strings.Join(items, "; ")
}

// HandbackVerdict is the unfixed verdict of a typed hand-back.
func HandbackVerdict(h *wire.Handback) string {
	if v := laneVerdict(h.Footer()); v != "none" || h.Status == "DONE" {
		return v
	}
	return h.Status // a decorated "none." item never clears an unfinished run
}

// leafReport is the leaf's SubagentHandback message, else its final assistant text.
func leafReport(transcript string) string {
	for _, line := range slices.Backward(bytes.Split(quality.TailOf(transcript), []byte("\n"))) {
		role, blocks, ok := quality.TurnEntry(line)
		if !ok || role != "assistant" {
			continue
		}
		for _, b := range slices.Backward(blocks) {
			var hb struct{ Message string }
			if b.Name == "SubagentHandback" && json.Unmarshal(b.Input, &hb) == nil && hb.Message != "" {
				return hb.Message
			}
		}
	}
	text, _ := quality.LastTurn(transcript)
	return text
}

// laneQueueMax caps the queue lines one prompt shows.
const laneQueueMax = 5

// laneTipTime is the commit time of the lane's branch tip (0 when unknown); a seam so tests need no git.
var laneTipTime = func(lane string) int64 {
	out, err := exec.Command("git", "-C", lane, "log", "-1", "--format=%ct").Output()
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || perr != nil {
		return 0
	}
	return n
}

// liveQueued is this repo's queued lanes minus those whose branch tip moved after the entry was written.
func liveQueued(main string) []worktree.QueuedLane {
	return slices.DeleteFunc(worktree.Queued(main), func(q worktree.QueuedLane) bool {
		return !paths.IsDir(q.Lane) || laneTipTime(q.Lane) > q.QueuedAt
	})
}

func laneQueueKey(q worktree.QueuedLane) string {
	return textutil.SHA256Hex(fmt.Appendf(nil, "%s|%s|%s|%s", q.Lane, q.Branch, q.Reason, q.Detail))[:16]
}

// freshQueueLines is every lane's key, the lines of lanes not in prev (capped) and how many were not in prev.
func freshQueueLines(lanes []worktree.QueuedLane, prev []string) (keys, lines []string, fresh int) {
	for _, q := range lanes {
		keys = append(keys, laneQueueKey(q))
		if slices.Contains(prev, keys[len(keys)-1]) {
			continue
		}
		if fresh++; fresh <= laneQueueMax {
			lines = append(lines, assets.HookLaneQueueLine(filepath.Base(q.Lane), q.Branch, q.Reason, q.Detail, q.Next))
		}
	}
	return keys, lines, fresh
}

// laneQueueContext shows a queued lane's line once per session, then one count line when the queue changes.
func laneQueueContext(cwd, session string) string {
	if !paths.FileExists(worktree.LandQueueFile()) {
		return ""
	}
	top, main := paths.CheckoutOf(cmp.Or(cwd, "."))
	if top == "" {
		return ""
	}
	lanes := liveQueued(paths.Canon(main))
	seenFile := filepath.Join(paths.StateDir(), "lane-queue-seen", worktree.SessionSlug(session))
	var prev []string
	if session != "" {
		raw, _ := os.ReadFile(seenFile)
		prev = strings.Fields(string(raw))
	}
	keys, lines, fresh := freshQueueLines(lanes, prev)
	if session != "" {
		if slices.Equal(keys, prev) {
			return ""
		}
		_ = os.MkdirAll(filepath.Dir(seenFile), 0o700)
		_ = paths.AtomicWriteNoSync(seenFile, []byte(strings.Join(keys, "\n")+"\n"), 0o600)
		if fresh == 0 && len(lanes) > 0 {
			lines = append(lines, fmt.Sprintf("land queue: %d lanes waiting; claude-code worktree list shows each", len(lanes)))
		}
	}
	if fresh > laneQueueMax {
		lines = append(lines, assets.HookLaneQueueMore(fresh-laneQueueMax))
	}
	return strings.Join(lines, "\n")
}
