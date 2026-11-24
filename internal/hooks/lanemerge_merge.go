package hooks

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// mergeLane re-inspects the lane under the machine-wide lane lock, then merges it; a moved target retries (bounded).
func mergeLane(j laneJob, repo *worktree.Repo, base string) {
	code := 1
	var log strings.Builder
	for try := range laneMergeTries {
		if try > 0 {
			time.Sleep(laneRetryWait)
		}
		a := mergeAttempt(j, repo, base)
		switch {
		case a.empty:
			laneDone(j, base)
			return
		case a.hold:
			queueLane(j, a.q)
			return
		}
		code = a.code
		log.WriteString(a.out)
		if !a.retry {
			break
		}
	}
	if code == 0 {
		laneMerged(j, base, log.String())
		return
	}
	logPath := base + ".log"
	_ = os.WriteFile(logPath, []byte(log.String()), 0o600) // kept until the lane merges
	laneFailed(j, repo, code, log.String(), logPath)
}

// attempt is one pass under the lane lock: a hold or an empty lane found on re-inspection, else the merge's outcome.
type attempt struct {
	q                  worktree.QueuedLane
	hold, empty, retry bool
	code               int
	out                string
}

// mergeAttempt re-inspects the lane under the machine-wide lane lock (a leaf may have resumed), then merges it.
func mergeAttempt(j laneJob, repo *worktree.Repo, base string) attempt {
	var a attempt
	ran, _ := paths.TryFileLock(filepath.Join(laneStateDir(), "serial.lock"), laneSerialWait, func() error {
		if a.q, a.hold, a.empty = inspectLane(j, repo, base); !a.hold && !a.empty {
			a.code, a.out = runWorktreeMerge(repo.Main, filepath.Base(j.lane), j.session)
		}
		return nil
	})
	if !ran {
		return attempt{code: 1, out: "lane-merge: another lane held the merge lock for " + laneSerialWait.String() + "\n", retry: true}
	}
	a.retry = strings.Contains(a.out, "target moved") || strings.Contains(a.out, "holds the merge lock")
	return a
}

// runWorktreeMerge is the merge verb itself, run from the main checkout with the session's id for its plan ticks.
var runWorktreeMerge = func(main, name, session string) (int, string) {
	bin, ok := mainSibling()
	if !ok {
		if bin, _ = paths.SelfBinary(); bin == "" {
			return 1, "lane-merge: claude-code binary not found"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), laneMergeTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "worktree", "merge", "--resolve-agent", laneResolveWith, name)
	cmd.Dir, cmd.Env, cmd.WaitDelay = main, append(os.Environ(), paths.SessionIDEnv+"="+session), 5*time.Second
	out, err := proc.CombinedOutputCapped(cmd)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		return 1, string(out) + "\n" + err.Error()
	}
	return 0, string(out)
}

// laneDone clears a lane that has nothing left to merge: its queue entry and the worker's own files.
func laneDone(j laneJob, base string) {
	if err := worktree.Dequeue(j.lane); err != nil && j.session != "" {
		text := "lane " + filepath.Base(j.lane) + " is done but its merge-queue entry stays: " + err.Error()
		_ = worktree.Append(j.session, worktree.Notice{TS: time.Now().Unix(), Kind: "lane-merge", From: "lane-merge", Text: text})
	}
	for _, f := range []string{base + ".verdicts", base + ".log"} {
		_ = os.Remove(f)
	}
}

// laneMerged tells the session the lane landed: the promoted range, ticked tasks and whether the lane is gone.
func laneMerged(j laneJob, base, out string) {
	laneDone(j, base)
	if j.session == "" {
		return
	}
	var keep []string
	for line := range strings.Lines(out) {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "promoted:") || strings.Contains(strings.ToLower(l), "ticked") {
			keep = append(keep, strings.Join(strings.Fields(l), " "))
		}
	}
	text := assets.HookLaneMerged(filepath.Base(j.lane), capText(strings.Join(keep, "; ")), !paths.IsDir(j.lane))
	_ = worktree.Append(j.session, worktree.Notice{TS: time.Now().Unix(), Kind: "lane-merge", From: "lane-merge", Text: text})
}

var conflictFiles = regexp.MustCompile(`conflict(?: in \S+)?:\s+(.+?)(?:; |\n|$)`)

// laneFailed queues a lane whose merge stopped: an unresolved conflict names its files, a failed gate its log.
func laneFailed(j laneJob, repo *worktree.Repo, code int, out, logPath string) {
	merge := laneMergeCmd(j.lane)
	q := worktree.QueuedLane{Repo: repo.Main, Task: j.task}
	switch m := conflictFiles.FindStringSubmatch(out); {
	case code == 3 || m != nil && strings.Contains(out, "resolv"):
		q.Reason, q.Next = "conflict unresolved", "resolve it in "+j.lane+", then "+merge+" (or --resume)"
		if m != nil {
			q.Detail = capText(m[1])
		}
	case strings.Contains(out, "verify failed"):
		q.Reason, q.Detail, q.Next = "gate failed", logPath, "read the log, fix in "+j.lane+", then "+merge
	default:
		q.Reason, q.Detail, q.Next = "merge failed", capText(lastLine(out))+" (log "+logPath+")", merge
	}
	queueLane(j, q)
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range slices.Backward(lines) {
		if strings.Contains(l, "worktree merge:") {
			return strings.TrimSpace(l)
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
