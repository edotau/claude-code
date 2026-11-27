// The stuck-leaf watchdog's hook side: a throttled job check on the main session's turns, one resume per dead job.
package dispatchhook

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

const (
	watchEvery     = time.Minute        // at most one check per session per this
	watchBeatMax   = 4                  // healthy jobs named in the heartbeat line
	watchBeatLen   = 200                // the heartbeat line's length cap
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
	watchWait   = 4 * time.Second // the join cuts the check here (worktree-busy budget 5s)
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

// BusyWatched is worktree-busy plus the watchdog check (UserPromptSubmit).
func BusyWatched(in *core.Input) core.Result { return watched(wthook.Busy, in) }

// StopWatched is the docs-plan Stop slot, which only hosts the watchdog check.
func StopWatched(in *core.Input) core.Result { return watchTick(in) }

// watched runs r beside the watchdog check; a check past watchWait is cut and records nothing it did not return.
func watched(r core.Runner, in *core.Input) core.Result {
	deadline, g := time.Now().Add(watchWait), &tickGate{}
	defer time.AfterFunc(watchWait, g.stop).Stop() // cuts even while the runner is still running
	tick := make(chan tickResult, 1)               // buffered: a cut tick never blocks its goroutine
	go func() {
		defer func() { _ = recover() }() // a check panic costs only its own note
		tick <- watchTickGated(in, g)
	}()
	res := r(in) // the dispatcher recovers the runner's own panic
	var w core.Result
	select {
	case t := <-tick:
		if g.live() && time.Now().Before(deadline) {
			w = t.Result
			if t.commit != nil {
				w.Stderr = core.JoinNonEmpty("\n", w.Stderr, errText("watch", t.commit()))
			}
			break
		}
		w.Stderr = "watch: check cut at the hook budget"
	case <-time.After(time.Until(deadline)):
		g.stop()
		w.Stderr = "watch: check cut at the hook budget"
	}
	res.Context = core.JoinContext(res.Context, w.Context)
	res.SystemMessage = core.JoinNonEmpty("; ", res.SystemMessage, w.SystemMessage)
	res.Stderr = core.JoinNonEmpty("\n", res.Stderr, w.Stderr)
	return res
}

// tickGate cuts a late tick: it starts no resume (one launching is marked) and its seen keys are never committed.
type tickGate struct{ cut atomic.Bool }

func (g *tickGate) stop() { g.cut.Store(true) }

func (g *tickGate) live() bool { return g == nil || !g.cut.Load() }

func stderrTick(text string) (t tickResult) {
	t.Stderr = text
	return t
}

// tickResult is a check's note plus the write that records what it reported as seen.
type tickResult struct {
	core.Result
	commit func() error
}

// watchTick checks the main session's jobs once per watchEvery and reports each stale job once per reason.
func watchTick(in *core.Input) core.Result {
	t := watchTickGated(in, nil)
	if t.commit != nil {
		t.Stderr = core.JoinNonEmpty("\n", t.Stderr, errText("watch", t.commit()))
	}
	return t.Result
}

func watchTickGated(in *core.Input, g *tickGate) tickResult {
	mode := watchMode()
	if mode == "off" || in.Agent() != core.AgentClaude || in.SubagentID() != "" || in.StopHookActive {
		return tickResult{}
	}
	owner := watchOwner()
	if owner <= 1 {
		return tickResult{}
	}
	key, now := watchKey(in.SessionID, owner), watchNow()
	if err := os.MkdirAll(agent.WatchDir(), 0o700); err != nil {
		return stderrTick(errText("watch", err))
	}
	var res tickResult
	_, err := paths.TryFileLock(filepath.Join(agent.WatchDir(), key+".lock"), 0, func() error {
		res = watchTickLocked(key, owner, now, mode, g)
		return nil
	})
	res.Stderr = core.JoinNonEmpty("\n", res.Stderr, errText("watch", err))
	return res
}

func watchTickLocked(key string, owner int, now time.Time, mode string, g *tickGate) tickResult {
	if due, err := claimTick(key, now); !due {
		return stderrTick(errText("watch", err))
	}
	rep, err := watchJobs(owner, now, agent.WatchLimitsFromEnv())
	lines, commit, werr := freshLines(key, rep.Stale, mode, g)
	res := stderrTick(core.JoinNonEmpty("\n", errText("watch", err), errText("watch", werr)))
	res.commit = commit
	if len(lines) > 0 {
		res.Context = strings.Join(lines, "\n")
		res.SystemMessage = fmt.Sprintf("watch: %d stale job line(s); claude-code agents watch --once", len(lines))
	}
	if hb := runningLine(rep.Running, rep.Stale, now); hb != "" {
		res.Context = core.JoinNonEmpty("\n", res.Context, core.JoinNonEmpty("; ", hb, degradedNote("", "")))
	}
	return res
}

// runningDetail is a named job's progress (agent, elapsed, last event, tools); a job past the shown ones stays short.
func runningDetail(r agent.Record, shown int, now time.Time) string {
	if shown >= watchBeatMax { // never named in the line: skip its file reads
		return r.Agent + ", " + agent.ShortDur(now.Sub(r.Time))
	}
	p := agent.ProgressOf(r, now)
	p.Model, p.Doing = "", "" // the line holds up to four jobs: keep each to its signal
	return p.Line(60)
}

// runningLine names up to watchBeatMax healthy running jobs so a long healthy job is not silence; "" when none.
func runningLine(running []agent.Record, stale []agent.StaleJob, now time.Time) string {
	var parts []string
	for _, r := range running {
		if slices.ContainsFunc(stale, func(s agent.StaleJob) bool { return s.ID == r.ID }) {
			continue
		}
		what := "Job " + r.ID
		if t := recordPlanTask([]agent.Record{r}); t != "" {
			what = "Task " + t
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", what, runningDetail(r, len(parts), now)))
	}
	if len(parts) == 0 {
		return ""
	}
	n, line := min(len(parts), watchBeatMax), ""
	for ; n > 0; n-- { // drop entries until the line fits
		shown := parts[:n]
		if n < len(parts) {
			shown = append(slices.Clone(shown), fmt.Sprintf("+%d more", len(parts)-n))
		}
		if line = "running: " + strings.Join(shown, "; "); len(line) <= watchBeatLen {
			break
		}
	}
	return line
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
		if strings.HasSuffix(e.Name(), ".lock") {
			continue // Unlinking a held lock would let another hook lock a different inode.
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > watchKeep {
			if err := os.Remove(filepath.Join(agent.WatchDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// freshLines is a line per unreported stale job and reason, plus the seen write to commit once they are returned.
func freshLines(key string, stale []agent.StaleJob, mode string, g *tickGate) ([]string, func() error, error) {
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
			lines, add = append(lines, watchLine(s, mode, g)), append(add, k)
		}
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("watch: %d more stale job(s): claude-code agents watch --once", more))
	}
	if len(add) == 0 {
		return lines, nil, seenErr
	}
	return lines, func() error { return writeWatch(seenPath, []byte(strings.Join(add, "\n")+"\n"), os.O_APPEND) }, seenErr
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
func watchLine(s agent.StaleJob, mode string, g *tickGate) string {
	head := fmt.Sprintf("watch: job %s (%s, %s) %s, age %s, silent %s", s.Task, s.ID, s.Agent, reasonText[s.Reason],
		agent.ShortDur(s.Age), agent.ShortDur(s.Silence))
	if s.LastLine != "" && s.Reason == agent.WatchSilent {
		head += "; last log line: " + s.LastLine
	}
	if rec, err := loadDispatch(s.ID); err == nil { // R34: no resume, no resend line while the task is degraded
		if n := degradedNote(rec.Request.Prompt, rec.PlanTask); n != "" {
			return head + "; " + n
		}
	}
	if s.Resumable() && (mode == "dry" || g.live()) { // a cut tick starts nothing it cannot report
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
	_, nonce, err := agent.ClaimResend(s.ID)
	if err != nil { // the one takeover gate: live, cancelled, budget and a rival claim
		if errors.Is(err, agent.ErrHolderLive) || errors.Is(err, agent.ErrNotResent) {
			return "not resumed (" + err.Error() + "); next: claude-code agents result " + s.ID
		}
		return fmt.Sprintf("already resumed once or unmarkable (%v); next: claude-code agents result %s", err, s.ID)
	}
	if err := agent.MarkLaunching(s.ID, nonce); err != nil { // nothing launched yet: the claim can go
		err = errors.Join(err, agent.ReleaseResend(s.ID, nonce))
		return fmt.Sprintf("the one resume failed (%v); not retried. Resume by hand: %s", err, cmd)
	}
	// From here a launch may have happened whatever the outcome: the claim stays launching until the successor adopts it.
	id, err := startResume(s.Next, nonce)
	if err != nil {
		return fmt.Sprintf("the one resume failed (%v); not retried. Resume by hand: %s", err, cmd)
	}
	return fmt.Sprintf("resumed once as %s; next: claude-code agents result -f %s", id, id)
}

var jobStartedRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^job (\S+) started`) })

// launchResume runs the resume argv through this binary with the claim's nonce; `ask --background` detaches the job.
func launchResume(argv []string, nonce string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeLaunchIn)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, argv[1:]...)
	cmd.Env = append(os.Environ(), agent.ResumeNonceEnv+"="+nonce)
	out, err := cmd.Output()
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
	if !strings.Contains(cmd, "claude-code") && !strings.Contains(cmd, paths.HookBinaryName) {
		return nil
	}
	ids := append(ownIDs(cmp.Or(in.Nested("tool_response", "stdout"), in.ToolResponseText())), namedJobs(cmd)...)
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

// namedJobs is every literal dispatch id the command's `claude-code agents result|wait` segments name.
func namedJobs(cmd string) []string {
	var ids []string
	for _, toks := range relayCommands(cmd) {
		toks = agentCommand(toks)
		if len(toks) < 3 || toks[0] != "agents" || toks[1] != "result" && toks[1] != "wait" {
			continue
		}
		for _, a := range toks[2:] {
			if jobArgRe().MatchString(a) {
				ids = append(ids, a)
			}
		}
	}
	return ids
}
