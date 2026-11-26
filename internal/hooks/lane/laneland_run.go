package lane

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
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// landLane re-inspects the lane under the machine-wide lane lock, then lands it; a moved target retries (bounded).
func landLane(j laneJob, repo *worktree.Repo, base string) {
	code := 1
	var log strings.Builder
	for try := range laneLandTries {
		if try > 0 {
			time.Sleep(laneRetryWait)
		}
		a := landAttempt(j, repo, base)
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
		laneLanded(j, base, log.String())
		return
	}
	logPath := base + ".log"
	_ = os.WriteFile(logPath, []byte(log.String()), 0o600) // kept until the lane lands
	laneFailed(j, repo, code, log.String(), logPath)
}

// attempt is one pass under the lane lock: a hold or an empty lane found on re-inspection, else the land's outcome.
type attempt struct {
	q                  worktree.QueuedLane
	hold, empty, retry bool
	code               int
	out                string
}

// landAttempt re-inspects the lane under the machine-wide lane lock (a leaf may have resumed), then lands it unlocked.
func landAttempt(j laneJob, repo *worktree.Repo, base string) attempt {
	var a attempt
	ran, _ := paths.TryFileLock(filepath.Join(laneStateDir(), "serial.lock"), laneSerialWait, func() error {
		a.q, a.hold, a.empty = inspectLane(j, repo, base)
		return nil
	})
	if !ran {
		return attempt{code: 1, out: "lane-land: another lane held the land lock for " + laneSerialWait.String() + "\n", retry: true}
	}
	if !a.hold && !a.empty {
		a.code, a.out = runWorktreeLand(repo.Main, filepath.Base(j.lane), j.session)
	}
	a.retry = strings.Contains(a.out, "target moved") || strings.Contains(a.out, "holds the land lock")
	return a
}

// runWorktreeLand runs `worktree land` from the main checkout with the session's id.
var runWorktreeLand = func(main, name, session string) (int, string) {
	bin, ok := core.MainSibling()
	if !ok {
		if bin, _ = paths.SelfBinary(); bin == "" {
			return 1, "lane-land: claude-code binary not found"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), laneLandTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "worktree", "land", "--resolve-agent", laneResolveWith, name)
	cmd.Dir, cmd.Env, cmd.WaitDelay = main, append(os.Environ(), paths.SessionIDEnv+"="+session, worktree.LandWaitEnv+"=lane-land hook"), 5*time.Second
	out, err := proc.CombinedOutputCapped(cmd)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		return 1, string(out) + "\n" + err.Error()
	}
	return 0, string(out)
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

// laneLanded tells the session the lane landed and whether the lane is gone.
func laneLanded(j laneJob, base, out string) {
	laneDone(j, base)
	if j.session == "" {
		return
	}
	var keep []string
	for line := range strings.Lines(out) {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "landed ") {
			keep = append(keep, strings.Join(strings.Fields(l), " "))
		}
	}
	laneNotice(j.session, assets.HookLaneLanded(filepath.Base(j.lane), capText(strings.Join(keep, "; ")), !paths.IsDir(j.lane)))
}

var conflictFiles = regexp.MustCompile(`conflict(?: in \S+)?:\s+(.+?)(?:; |\n|$)`)

// laneFailed queues a lane whose land stopped: an unresolved conflict names its files, a failed gate its log.
func laneFailed(j laneJob, repo *worktree.Repo, code int, out, logPath string) {
	land := laneLandCmd(j.lane)
	q := worktree.QueuedLane{Repo: repo.Main, Task: j.task}
	switch m := conflictFiles.FindStringSubmatch(out); {
	case strings.Contains(out, worktree.ErrLandRefused.Error()):
		q.Reason, q.Detail, q.Next = "land refused: live tree dirty", capText(lastLine(out)), "commit or move those edits, then "+land
	case code == 3 || m != nil && strings.Contains(out, "resolv"):
		q.Reason, q.Next = "conflict unresolved", "resolve it in "+j.lane+", then "+land+" (or --resume)"
		if m != nil {
			q.Detail = capText(m[1])
		}
	case strings.Contains(out, "verify failed"):
		q.Reason, q.Detail, q.Next = "gate failed", logPath, "read the log, fix in "+j.lane+", then "+land
	default:
		q.Reason, q.Detail, q.Next = "land failed", capText(lastLine(out))+" (log "+logPath+")", land
	}
	queueLane(j, q)
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, l := range slices.Backward(lines) {
		if strings.Contains(l, "worktree land:") {
			return strings.TrimSpace(l)
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}
