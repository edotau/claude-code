package hooks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// laneMergeArg starts the detached worker (lanemerge_worker.go): `leaf <lane> <session> <transcript>`.
const laneMergeArg = "--internal-lane-merge"

// A private detached entry, like the bench worker's: it outlives the hook's budget.
func init() {
	switch {
	case len(os.Args) == 6 && os.Args[1] == laneMergeArg && os.Args[2] == "leaf":
		runLeafLane(os.Args[3], os.Args[4], os.Args[5])
		os.Exit(0)
	case len(os.Args) == 3 && os.Args[1] == agent.LaneMergeJobArg:
		runJobLane(os.Args[2])
		os.Exit(0)
	}
}

// laneSpawn starts the detached worker outside any lane; a seam so tests spawn nothing.
var laneSpawn = func(args []string) bool {
	self, err := paths.SelfBinary()
	return err == nil && proc.SpawnDetached(self, args, paths.StateDir(), nil)
}

// LaneMergeQueue (UserPromptSubmit) lists this repo's queued lanes, each with its next command.
func LaneMergeQueue(in *core.Input) core.Result {
	return core.Result{Context: laneQueueContext(in.CWD)}
}

// LaneMerge (SubagentStop) hands a stopped leaf's lane to the detached worker and returns at once.
func LaneMerge(in *core.Input) core.Result {
	if in.AgentID == "" || in.AgentTranscriptPath == "" {
		return core.Result{}
	}
	lane := leafLane(in)
	if lane == "" {
		return core.Result{}
	}
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	if !laneSpawn([]string{laneMergeArg, "leaf", lane, in.SessionID, in.AgentTranscriptPath}) {
		return core.Result{Stderr: "lane-merge: could not start the merge worker for " + lane}
	}
	return core.Result{}
}

// leafLane is the linked worktree the leaf was spawned in (meta.json), else the one lane all its claims sit in.
func leafLane(in *core.Input) string {
	var meta struct {
		WorktreePath string `json:"worktreePath"`
	}
	if raw, err := os.ReadFile(leafMeta(in.AgentTranscriptPath)); err == nil && json.Unmarshal(raw, &meta) == nil && meta.WorktreePath != "" {
		return laneOf(meta.WorktreePath)
	}
	return claimedLane(in.SessionID, in.AgentID)
}

// claimedLane is the one lane the claims ledger shows agentID writing in; none or several is no guess.
func claimedLane(session, agentID string) string {
	file := filepath.Join(worktree.ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", inflight.SessionOwner(), worktree.SessionSlug(session)))
	var lanes []string
	for line := range bytes.SplitSeq(tailOf(file), []byte("\n")) {
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

func leafMeta(transcript string) string {
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
	items := unfixedItems(report)
	switch {
	case len(items) == 0:
		return "missing"
	case unfixedNone(items):
		return "none"
	}
	return strings.Join(items, "; ")
}

// leafReport is the leaf's SubagentHandback message, else its final assistant text.
func leafReport(transcript string) string {
	for _, line := range slices.Backward(bytes.Split(tailOf(transcript), []byte("\n"))) {
		role, blocks, ok := turnEntry(line)
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
	text, _ := lastTurn(transcript)
	return text
}

// laneQueueMax caps the queue lines one prompt shows.
const laneQueueMax = 5

// laneQueueContext is one line per lane queued for this checkout's repo; "" (one stat) when nothing is queued.
func laneQueueContext(cwd string) string {
	if !paths.FileExists(worktree.MergeQueueFile()) {
		return ""
	}
	top, main := paths.CheckoutOf(cmp.Or(cwd, "."))
	if top == "" {
		return ""
	}
	lanes := worktree.Queued(paths.Canon(main))
	var lines []string
	for i, q := range lanes {
		if i == laneQueueMax {
			lines = append(lines, assets.HookLaneQueueMore(len(lanes)-laneQueueMax))
			break
		}
		lines = append(lines, assets.HookLaneQueueLine(filepath.Base(q.Lane), q.Branch, q.Reason, q.Detail, q.Next))
	}
	return strings.Join(lines, "\n")
}
