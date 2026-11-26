package lane

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// laneJob is one finished run in a lane: who to tell, its UNFIXED verdict, its plan task, and a still-done check.
type laneJob struct {
	lane, session, verdict, task, job string      // job: the background ask's id, "" for a leaf
	done                              func() bool // the run is still finished; rechecked right before the land
}

// Bounds of the detached worker; vars so tests run in milliseconds.
var (
	laneFinishWait   = 2 * time.Hour // a held leaf stop keeps the leaf running up to leafStopMaxWait
	laneExitWait     = time.Minute
	laneFinishPoll   = 2 * time.Second
	laneQueueBudget  = 10 * time.Second // the queue lock budget (worktree queueEditWait)
	laneQueueAttempt = time.Second
	laneQueuePause   = 200 * time.Millisecond
	laneSerialWait   = time.Hour // another lane's judgement or promotion runs first, never concurrently
)

// laneResolveWith settles a rebase conflict in the lane (worktree-git.md → Land on completion).
const laneResolveWith = "pi"

// laneUnfinished is the verdict of a run that never showed it finished: queue it, never land it.
const laneUnfinished = "unfinished"

func laneStateDir() string {
	dir := filepath.Join(paths.StateDir(), "worktree")
	return paths.MoveLegacy(filepath.Join(dir, "lane-merge"), filepath.Join(dir, "lane-land"))
}

// runLeafLane waits for the leaf to finish (a held stop keeps it working), then judges its lane.
func runLeafLane(lane, session, transcript string) {
	finished := func() bool { return leafFinished(transcript) }
	verdict := laneUnfinished
	done := finished
	if waitUntil(laneFinishWait, finished) {
		seen := transcriptStamp(transcript) // taken before the read, so a write in between holds the lane
		verdict = laneVerdict(leafReport(transcript))
		done = func() bool { return finished() && transcriptStamp(transcript) == seen } // a leaf sent back writes more
	}
	runLaneJob(laneJob{lane: lane, session: session, verdict: verdict, done: done})
}

// transcriptStamp is the transcript's mtime and size; it changes when the leaf runs again.
func transcriptStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fi.ModTime().String() + strconv.FormatInt(fi.Size(), 10)
}

// leafFinished is Claude Code's finished mark in the leaf's meta.json; no readable mark is not finished.
func leafFinished(transcript string) bool {
	raw, err := os.ReadFile(LeafMeta(transcript))
	var m struct{ Finished bool }
	return err == nil && json.Unmarshal(raw, &m) == nil && m.Finished
}

// runJobLane judges the lane a finished background ask job worked in, once its process has exited.
func runJobLane(id string) {
	rec, err := agent.LoadRecord(id)
	if err != nil || rec.Tree == "" {
		return
	}
	lane := laneOf(rec.Tree)
	if lane == "" {
		return
	}
	done := func() bool { return jobFinished(rec) }
	verdict := laneUnfinished
	if waitUntil(laneExitWait, done) {
		env, _ := rec.SavedEnvelope()
		verdict = laneVerdict(env.Answer)
		if env.Handback != nil && env.Error == nil { // the typed outcome wins over the answer's text line
			verdict = HandbackVerdict(env.Handback)
		}
	}
	runLaneJob(laneJob{lane: lane, session: os.Getenv(paths.SessionIDEnv), verdict: verdict, task: rec.PlanTask, job: rec.ID,
		done: done})
}

// jobFinished reloads the record each call, so a job resumed later under a new PID is not seen as finished.
func jobFinished(rec agent.Record) bool { return !agent.RecordLive(rec) }

func waitUntil(limit time.Duration, done func() bool) bool {
	for end := time.Now().Add(limit); !done(); time.Sleep(laneFinishPoll) {
		if time.Now().After(end) {
			return false
		}
	}
	return true
}

// runLaneJob records the run's verdict, then judges the lane once every earlier trigger for it is judged.
func runLaneJob(j laneJob) {
	if os.MkdirAll(laneStateDir(), 0o700) != nil {
		return
	}
	base := filepath.Join(laneStateDir(), worktree.LaneInbox(j.lane))
	line := strconv.FormatInt(time.Now().Unix(), 10) + " " + j.verdict
	if err := paths.AppendLine(base+".verdicts", []byte(line)); err != nil {
		j.verdict = "verdict not recorded: " + err.Error()
	}
	ran, _ := paths.TryFileLock(base+".lock", laneSerialWait, func() error {
		judgeLane(j, base)
		return nil
	})
	if !ran {
		queueLane(j, worktree.QueuedLane{Lane: j.lane, Reason: "not judged: another lane-land worker held it",
			Next: laneLandCmd(j.lane)})
	}
}

// laneVerdictOf is the first non-clean verdict a run in this lane recorded since the lane was made, else own.
func laneVerdictOf(base, lane, own string) string {
	made := int64(0) // a lane re-made at the same path ignores the old lane's verdicts
	if fi, err := os.Stat(filepath.Join(lane, ".git")); err == nil {
		made = fi.ModTime().Unix()
	}
	raw, _ := os.ReadFile(base + ".verdicts")
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		at, v, _ := strings.Cut(strings.TrimSpace(string(line)), " ")
		if ts, err := strconv.ParseInt(at, 10, 64); err == nil && ts >= made && v != "" && v != "none" {
			return v
		}
	}
	return own
}

func judgeLane(j laneJob, base string) {
	if !paths.IsDir(j.lane) {
		laneDone(j, base) // already landed and removed
		lateVerdict(j)
		return
	}
	repo, err := worktree.ResolveRepo(j.lane)
	if err != nil {
		queueLane(j, worktree.QueuedLane{Lane: j.lane, Reason: "not a lane of a readable repo", Detail: err.Error()})
		return
	}
	q, hold, empty := inspectLane(j, repo, base)
	switch {
	case empty:
		laneDone(j, base)
	case hold:
		queueLane(j, q)
	default:
		q.Reason, q.QueuedAt = worktree.QueueReady, time.Now().Unix() // fresh: an older stamp hides it
		queueLane(j, q)                                               // the main session lands it; the worker never touches the live checkout
	}
}

// laneDone clears a lane that has nothing left to land: its queue entry and the worker's own files.
func laneDone(j laneJob, base string) {
	if err := worktree.Dequeue(j.lane); err != nil && j.session != "" {
		laneNotice(j.session, "lane "+filepath.Base(j.lane)+" is done but its land-queue entry stays: "+err.Error())
	}
	for _, f := range []string{base + ".verdicts", base + ".log"} {
		_ = os.Remove(f)
	}
}

// inspectLane reads the lane's state: empty when it has nothing to land, hold (with q filled) when it must wait.
var inspectLane = func(j laneJob, repo *worktree.Repo, base string) (q worktree.QueuedLane, hold, empty bool) {
	j.verdict = laneVerdictOf(base, j.lane, j.verdict) // re-read: a sibling run may have finished meanwhile
	q = worktree.QueuedLane{Lane: j.lane, Repo: repo.Main, Task: j.task}
	entries, err := worktree.List(repo)
	i := slices.IndexFunc(entries, func(e worktree.Entry) bool { return paths.SamePath(e.Path, j.lane) })
	if err != nil || i < 0 {
		q.Reason, q.Next = "lane not listed", laneLandCmd(j.lane)
		return q, true, false
	}
	e := entries[i]
	q.Branch = e.Branch
	if e.Ahead == 0 && !e.Dirty && len(e.Problems) == 0 && !e.DivergenceUnknown() {
		return q, false, true // nothing to land: no commits ahead, nothing uncommitted
	}
	return q, laneHold(j, e, repo, &q), false
}

// laneHold fills q's reason and next step when the lane must not land by itself; false lets it land.
func laneHold(j laneJob, e worktree.Entry, repo *worktree.Repo, q *worktree.QueuedLane) bool {
	if verdictHold(j, q) {
		return true
	}
	land := laneLandCmd(j.lane)
	switch {
	case len(e.Problems) > 0 || e.Busy != "" || e.DivergenceUnknown():
		why := cmp.Or(e.Busy, strings.Join(e.Problems, "; "), "divergence unknown")
		q.Reason, q.Detail, q.Next = "lane busy or unreadable", capText(why), land+" once it is free"
	default:
		hit := restrictionFiles(laneChanged(repo, j.lane))
		if len(hit) == 0 {
			return false
		}
		q.Reason, q.Detail = "adds a restriction, waits for the owner's yes", capText(strings.Join(hit, ", "))
		q.Next = "after the owner's yes: " + land
	}
	return true
}

// verdictHold holds a lane whose run is not finished or whose report is not a clean `UNFIXED: none`.
func verdictHold(j laneJob, q *worktree.QueuedLane) bool {
	land := laneLandCmd(j.lane)
	switch {
	case j.job != "" && j.verdict == laneUnfinished:
		q.Reason = "job cut off before it finished"
		q.Next = "continue it: claude-code agents ask --retry " + j.job + " --recover --runner pi|cli, or " + land
	case j.verdict == laneUnfinished || j.done != nil && !j.done():
		q.Reason, q.Next = "leaf or job not finished", "once it hands back, "+land
	case j.verdict == "missing":
		q.Reason, q.Next = "leaf report has no UNFIXED line", "read the report, finish the work in "+j.lane+", then "+land
	case j.verdict != "none":
		q.Reason, q.Detail, q.Next = "unfixed items", capText(j.verdict), "fix them in "+j.lane+", then "+land
	default:
		return false
	}
	return true
}

// laneLandCmd is the land command for lane; flags go before the name.
func laneLandCmd(lane string) string {
	return "claude-code worktree land --resolve-agent " + laneResolveWith + " " + filepath.Base(lane)
}

// laneTextCap bounds a queue detail, so one lane's line stays one line.
const laneTextCap = 300

func capText(s string) string {
	return textutil.CapRunes(s, laneTextCap, "...")
}

// laneChanged is the files the lane's commits change against the main checkout's branch.
func laneChanged(repo *worktree.Repo, lane string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, ok, _ := proc.Git(ctx, repo.Main, "symbolic-ref", "--short", "HEAD")
	if !ok {
		return []string{"(land target unknown)"} // fail closed: no auto-land without the diff
	}
	files, ok, _ := proc.Git(ctx, lane, "diff", "--name-only", target[0]+"...HEAD")
	if !ok {
		return []string{"(diff unreadable)"}
	}
	return files
}

// restrictionFiles is the changed files that can add a block, deny or narrower sandbox (owner review).
func restrictionFiles(files []string) []string {
	return slices.DeleteFunc(slices.Clone(files), func(f string) bool {
		base := filepath.Base(f)
		return !strings.HasPrefix(f, "internal/hooks/safety/") && f != "internal/platform/settings/hookspec.go" &&
			(!strings.HasPrefix(base, "settings") || !strings.HasSuffix(base, ".json")) && !strings.HasPrefix(f, "(")
	})
}

// lateVerdict tells the session a run finished unclean after its lane already landed, so it is fixed on main.
func lateVerdict(j laneJob) {
	if j.verdict == "none" || j.session == "" {
		return
	}
	laneNotice(j.session, "lane "+filepath.Base(j.lane)+" landed before this run's report was judged; it reported: "+
		capText(j.verdict)+"; fix it on the working branch now")
}

// laneNotice appends a lane-land notice to the session's inbox; a failed append is dropped.
func laneNotice(session, text string) {
	_ = worktree.Append(session, worktree.Notice{TS: time.Now().Unix(), Kind: "lane-land", From: "lane-land", Text: text})
}

// queueLane writes q, retrying a busy queue lock with backoff; a lane still unqueued is broadcast, never dropped.
func queueLane(j laneJob, q worktree.QueuedLane) {
	q.Lane = j.lane
	q.Next = cmp.Or(q.Next, laneLandCmd(j.lane))
	err := enqueueRetry(q)
	if err == nil {
		return
	}
	if errors.Is(err, paths.ErrLockTimeout) {
		_, _ = worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{Kind: worktree.FailLaneGate,
			Subject: filepath.Base(j.lane), Summary: "not queued: land-queue lock busy", Log: q.Next})
	}
	if j.session != "" {
		laneNotice(j.session, "lane "+filepath.Base(j.lane)+" "+q.Reason+" but the land queue is unwritable ("+err.Error()+
			"); next: "+q.Next)
	}
}

// enqueueRetry tries short lock waits with doubling pauses until laneQueueBudget is spent.
func enqueueRetry(q worktree.QueuedLane) error {
	deadline, pause := time.Now().Add(laneQueueBudget), laneQueuePause
	for {
		err := worktree.EnqueueWithin(laneQueueAttempt, q)
		if err == nil || !errors.Is(err, paths.ErrLockTimeout) || time.Now().Add(pause).After(deadline) {
			return err
		}
		time.Sleep(pause)
		pause *= 2
	}
}
