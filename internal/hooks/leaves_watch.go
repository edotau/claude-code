// The stuck-leaf watchdog's hook side: a throttled job check on the main session's turns, one resume per dead job.
package hooks

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/inflight"
)

const (
	watchEvery     = time.Minute        // at most one check per session per this
	watchLinesMax  = 5                  // context lines per check; the rest wait for the next one
	watchKeep      = 7 * 24 * time.Hour // markers older than this are swept on a check
	resumeLaunchIn = 3 * time.Second    // the launcher prints its job id well inside the hook budget
)

// Seams: the clock, the session owner, the classifier and the resume launch; tests replace them.
var (
	watchNow    = time.Now
	watchOwner  = inflight.SessionOwner
	watchJobs   = agent.Watch
	startResume = launchResume
)

// watchMode is HARNESS_WATCH: "off" (off|false|0), "dry" (report the resume, start nothing), else "on".
func watchMode() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(agent.WatchEnv))); v {
	case "off", "false", "0":
		return "off"
	case "dry":
		return v
	}
	return "on"
}

// busyWatched is worktree-busy plus the watchdog check (UserPromptSubmit).
func busyWatched(in *core.Input) core.Result { return watched(wthook.Busy, in) }

// reminderWatched is docs-plan-reminder plus the watchdog check (Stop).
func reminderWatched(in *core.Input) core.Result { return watched(docs.Reminder, in) }

// watched runs r, then the watchdog check, joining their notes.
func watched(r core.Runner, in *core.Input) core.Result {
	res, w := r(in), watchTick(in)
	res.Context = core.JoinContext(res.Context, w.Context)
	res.SystemMessage = core.JoinNonEmpty("; ", res.SystemMessage, w.SystemMessage)
	res.Stderr = core.JoinNonEmpty("\n", res.Stderr, w.Stderr)
	return res
}

// watchTick checks the main session's jobs once per watchEvery and reports each stale job once per reason.
func watchTick(in *core.Input) core.Result {
	mode := watchMode()
	if mode == "off" || in.Agent() != core.AgentClaude || in.SubagentID() != "" || in.StopHookActive {
		return core.Result{}
	}
	owner := watchOwner()
	if owner <= 1 {
		return core.Result{}
	}
	key, now := watchKey(in.SessionID, owner), watchNow()
	if due, err := claimTick(key, now); !due {
		return core.Result{Stderr: errText("watch", err)}
	}
	rep, err := watchJobs(owner, now, agent.WatchLimitsFromEnv())
	lines, werr := freshLines(key, rep.Stale, mode)
	res := core.Result{Stderr: core.JoinNonEmpty("\n", errText("watch", err), errText("watch", werr))}
	if len(lines) > 0 {
		res.Context = strings.Join(lines, "\n")
		res.SystemMessage = fmt.Sprintf("watch: %d stale job line(s); claude-code agents watch --once", len(lines))
	}
	return res
}

func errText(prefix string, err error) string {
	if err == nil {
		return ""
	}
	return prefix + ": " + err.Error()
}

var safeKeyRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.-]+$`) })

// watchKey names the session's tick and seen files: its session id, else the owner pid.
func watchKey(session string, owner int) string {
	if safeKeyRe().MatchString(session) && session != "." && session != ".." {
		return session
	}
	return "pid-" + strconv.Itoa(owner)
}

// claimTick stamps the session's tick when the last one is at least watchEvery old; a fresh stamp is not due.
func claimTick(key string, now time.Time) (bool, error) {
	p := filepath.Join(agent.WatchDir(), key+".tick")
	if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < watchEvery && !info.ModTime().After(now) {
		return false, nil
	}
	if err := writeWatch(p, []byte(now.UTC().Format(time.RFC3339)+"\n"), os.O_TRUNC); err != nil {
		return false, err
	}
	if err := os.Chtimes(p, now, now); err != nil {
		return false, err
	}
	return true, pruneWatch(now)
}

// pruneWatch removes markers untouched for watchKeep.
func pruneWatch(now time.Time) error {
	entries, err := os.ReadDir(agent.WatchDir())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > watchKeep {
			if err := os.Remove(filepath.Join(agent.WatchDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// freshLines is one context line per stale job and reason not yet reported this session; it records them as seen.
func freshLines(key string, stale []agent.StaleJob, mode string) ([]string, error) {
	seenPath := filepath.Join(agent.WatchDir(), key+".seen")
	seen, seenErr := readSeen(seenPath)
	var lines, add []string
	more := 0
	for _, s := range stale {
		k := s.ID + " " + s.Reason
		switch {
		case seen[k]:
		case len(lines) == watchLinesMax:
			more++
		default:
			lines, add = append(lines, watchLine(s, mode)), append(add, k)
		}
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("watch: %d more stale job(s): claude-code agents watch --once", more))
	}
	if len(add) == 0 {
		return lines, seenErr
	}
	return lines, errors.Join(seenErr, writeWatch(seenPath, []byte(strings.Join(add, "\n")+"\n"), os.O_APPEND))
}

// readSeen is the keys already reported; a missing file is none, a read error comes back with what was read.
func readSeen(path string) (map[string]bool, error) {
	seen := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return seen, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		seen[sc.Text()] = true
	}
	return seen, sc.Err()
}

// watchLine is the context line for one stale job; a resumable dead job is resumed once first.
func watchLine(s agent.StaleJob, mode string) string {
	head := fmt.Sprintf("watch: job %s (%s, %s) %s, age %s, silent %s", s.Task, s.ID, s.Agent, reasonText[s.Reason],
		agent.ShortDur(s.Age), agent.ShortDur(s.Silence))
	if s.LastLine != "" && s.Reason == agent.WatchSilent {
		head += "; last log line: " + s.LastLine
	}
	if s.Resumable() {
		return head + "; " + resumeOnce(s, mode)
	}
	return head + "; next: " + strings.Join(s.Next, " ")
}

var reasonText = map[string]string{
	agent.WatchDead:        "died while its record says running",
	agent.WatchSilent:      "is alive but its log is silent past the limit (stuck or slow: read the tail)",
	agent.WatchRelayGone:   "still runs but the relay that launched it finished",
	agent.WatchUncollected: "finished but nobody collected its result",
}

// resumeOnce starts the one watchdog resume of a dead job, claiming its marker first so it never runs twice.
func resumeOnce(s agent.StaleJob, mode string) string {
	cmd := strings.Join(s.Next, " ")
	if mode == "dry" {
		return "would resume once (HARNESS_WATCH=dry): " + cmd
	}
	marker := agent.WatchMarker("resumed", s.ID)
	if marker == "" {
		return "next: " + cmd
	}
	if err := writeWatch(marker, nil, os.O_EXCL); err != nil {
		return fmt.Sprintf("already resumed once or unmarkable (%v); next: claude-code agents result %s", err, s.ID)
	}
	id, err := startResume(s.Next)
	if err != nil {
		return fmt.Sprintf("the one resume failed (%v); not retried. Resume by hand: %s", err, cmd)
	}
	if werr := writeWatch(marker, []byte(id+"\n"), os.O_TRUNC); werr != nil {
		return fmt.Sprintf("resumed once as %s (marker not saved: %v); next: claude-code agents result -f %s", id, werr, id)
	}
	return fmt.Sprintf("resumed once as %s; next: claude-code agents result -f %s", id, id)
}

var jobStartedRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^job (\S+) started`) })

// launchResume runs the resume argv through this binary; `ask --background` detaches the job and prints its id.
func launchResume(argv []string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeLaunchIn)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, argv[1:]...).Output()
	if m := jobStartedRe().FindSubmatch(out); m != nil {
		return string(m[1]), nil
	}
	return "", cmp.Or(err, errors.New("no job id in the launch output"))
}

// writeWatch writes one watchdog file (0600) with flag (os.O_TRUNC, O_APPEND or O_EXCL), creating its dir.
func writeWatch(path string, data []byte, flag int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|flag, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	return errors.Join(werr, f.Close())
}

// markCollected marks every ended job a Bash call read (`agents result <id>`, a foreground ask), for the watchdog.
func markCollected(in *core.Input) error {
	cmd := in.Nested("tool_input", "command")
	if !strings.Contains(cmd, "claude-code") {
		return nil
	}
	ids := ownIDs(cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText()))
	if job, _, ok := pollJob(cmd); ok {
		ids = append(ids, job)
	}
	var errs []error
	for _, id := range ids {
		if id == "" {
			continue
		}
		rec, err := agent.LoadRecord(id)
		if err != nil || rec.Status == "running" {
			continue
		}
		if p := agent.WatchMarker("collected", rec.ID); p != "" {
			errs = append(errs, writeWatch(p, nil, os.O_TRUNC))
		}
	}
	return errors.Join(errs...)
}
