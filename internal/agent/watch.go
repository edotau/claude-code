// watch.go classifies a session's dispatch jobs as ok or stale for `agents watch` and the hook watchdog.
package agent

import (
	"bytes"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// Stale reasons, one per check.
const (
	WatchDead        = "dead"        // the record says running, its process is gone
	WatchSilent      = "silent"      // past the age limit and its log silent past the silence limit
	WatchRelayGone   = "relay-gone"  // the relay that launched it finished while it runs
	WatchUncollected = "uncollected" // finished, and nobody read its result
)

// ResumePrompt is the one continuation the watchdog sends a dead job's session.
const ResumePrompt = "You were interrupted. Continue the task from where you stopped and finish with the report the " +
	"brief asked for."

// Watch env names (vault env); defaults apply to an unset or unparseable value.
const (
	WatchEnv        = "HARNESS_WATCH"
	watchAgeEnv     = "HARNESS_WATCH_AGE"
	watchSilenceEnv = "HARNESS_WATCH_SILENCE"
)

const (
	defaultWatchAge     = 10 * time.Minute
	defaultWatchSilence = 5 * time.Minute
	uncollectedAfter    = 5 * time.Minute
	collectHorizon      = 2 * time.Hour // older finished jobs predate any collect marker: never flagged
)

// WatchLimits are the standard-tier age limit and the log-silence limit.
type WatchLimits struct{ Age, Silence time.Duration }

// WatchLimitsFromEnv reads HARNESS_WATCH_AGE and HARNESS_WATCH_SILENCE.
func WatchLimitsFromEnv() WatchLimits {
	return WatchLimits{Age: envDuration(watchAgeEnv, defaultWatchAge), Silence: envDuration(watchSilenceEnv, defaultWatchSilence)}
}

func envDuration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name))); err == nil && d > 0 {
		return d
	}
	return def
}

// ageFor scales the age limit by the brief's `Tier:` line: fast 0.5x, deep 2x.
func (l WatchLimits) ageFor(prompt string) time.Duration {
	m := tierLineRe().FindStringSubmatch(prompt)
	switch {
	case m == nil:
		return l.Age
	case strings.EqualFold(m[1], "fast"):
		return l.Age / 2
	case strings.EqualFold(m[1], "deep"):
		return l.Age * 2
	}
	return l.Age
}

// StaleJob is one job the watchdog flags, with the command that acts on it.
type StaleJob struct {
	ID        string        `json:"id"`
	Task      string        `json:"task"`
	Agent     string        `json:"agent"`
	Model     string        `json:"model"`
	Reason    string        `json:"reason"`
	Age       time.Duration `json:"age_ns"`
	Silence   time.Duration `json:"silence_ns"`
	LastLine  string        `json:"last_line,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Next      []string      `json:"next"` // argv of the next command, starting with claude-code
}

// Resumable is a dead job the watchdog may resume: it has a session to continue.
func (s StaleJob) Resumable() bool { return s.Reason == WatchDead && s.SessionID != "" }

// WatchReport is one pass: the stale jobs, and how many more may still turn stale.
type WatchReport struct {
	Stale   []StaleJob `json:"stale"`
	Pending int        `json:"pending"`
}

// watchAlive is the liveness probe; tests replace it.
var watchAlive = func(r Record) bool {
	if !r.Alive() {
		return false
	}
	if r.PIDStart == "" || r.PID <= 0 {
		return true
	}
	got, err := proc.StartTime(r.PID)
	return err != nil || got == r.PIDStart // a reused pid is not the job
}

// WatchDir holds the watchdog's markers: collected-<id>, resumed-<id>, <session>.tick and <session>.seen.
func WatchDir() string { return filepath.Join(paths.StateDir(), "watch") }

// WatchMarker is the marker path for kind ("collected", "resumed") and id; "" for an id unsafe in a file name.
func WatchMarker(kind, id string) string {
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return ""
	}
	return filepath.Join(WatchDir(), kind+"-"+id)
}

// ResumedAs is the job id the watchdog resumed id as; "" when it never did.
func ResumedAs(id string) (string, bool) {
	p := WatchMarker("resumed", id)
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err == nil
}

// Watch classifies owner's jobs (and the jobs the watchdog resumed for them) at now.
func Watch(owner int, now time.Time, lim WatchLimits) (WatchReport, error) {
	recs, err := ReadRecent(AllRecords)
	return classifyJobs(ownedJobs(recs, owner), now, lim, scanRelays), err
}

// ownedJobs is the records owner launched plus every job the watchdog resumed from one of them.
func ownedJobs(recs []Record, owner int) []Record {
	if owner <= 1 {
		return nil
	}
	mine := map[string]bool{}
	var out []Record
	for _, r := range slices.Backward(recs) { // oldest first, so a resume follows its original
		if r.Owner != owner && !mine[r.ID] {
			continue
		}
		out = append(out, r)
		if next, ok := ResumedAs(r.ID); ok {
			mine[next] = true
		}
	}
	return out
}

// relayState is what the relay transcript naming a job shows: whether it finished, and when it was last written.
type relayState struct {
	finished bool
	at       time.Time
}

// classifyJobs flags each stale job; relays maps job ids to their relay transcripts' state.
func classifyJobs(recs []Record, now time.Time, lim WatchLimits, relays func([]Record) map[string]relayState) WatchReport {
	var rep WatchReport
	var stale []StaleJob
	rel := relays(recs)
	for _, r := range recs {
		s, pending := classifyJob(r, now, lim, rel[r.ID])
		if s.Reason != "" {
			stale = append(stale, s)
		}
		if pending {
			rep.Pending++
		}
	}
	rep.Stale = stale
	return rep
}

// classifyJob is one record's verdict: a stale entry (empty Reason when ok) and whether it may still turn stale.
func classifyJob(r Record, now time.Time, lim WatchLimits, rel relayState) (StaleJob, bool) {
	if r.Status == "running" {
		return classifyRunning(r, now, lim, rel)
	}
	end := r.Time.Add(time.Duration(r.DurationMs) * time.Millisecond)
	if now.Sub(end) > collectHorizon || collected(r.ID) || rel.finished && !rel.at.Before(end) {
		return StaleJob{}, false
	}
	if now.Sub(end) <= uncollectedAfter {
		return StaleJob{}, true
	}
	s := staleFrom(r, now, WatchUncollected)
	s.Next = []string{"claude-code", "agents", "result", r.ID}
	return s, false
}

// classifyRunning checks a running record: a dead process at once, else a gone relay, else age and silence.
func classifyRunning(r Record, now time.Time, lim WatchLimits, rel relayState) (StaleJob, bool) {
	if !watchAlive(r) {
		if _, done := ResumedAs(r.ID); done {
			return StaleJob{}, false // the resumed job carries the work on
		}
		s := staleFrom(r, now, WatchDead)
		s.SessionID = jobSession(r)
		s.Next = []string{"claude-code", "agents", "result", r.ID}
		if s.SessionID != "" {
			s.Next = ResumeArgs(r, s.SessionID)
		}
		return s, false
	}
	follow := []string{"claude-code", "agents", "result", "-f", r.ID}
	if rel.finished {
		s := staleFrom(r, now, WatchRelayGone)
		s.Next = follow
		return s, false
	}
	s := staleFrom(r, now, WatchSilent)
	if s.Age <= lim.ageFor(r.Request.Prompt) || s.Silence <= lim.Silence {
		return StaleJob{}, true
	}
	s.Next = []string{"claude-code", "agents", "result", r.ID}
	return s, false
}

// ResumeArgs is the `agents ask --session` argv that continues r's session in the background.
func ResumeArgs(r Record, session string) []string {
	args := []string{"claude-code", "agents", "ask", "--agent", cmp.Or(r.RanAs, r.Agent), "--session", session, "--background"}
	switch {
	case r.Request.ReadOnly:
		args = append(args, "--read-only")
	case r.Request.Tools:
		args = append(args, "--tools")
	}
	if r.Request.WorkDir != "" {
		args = append(args, "--workdir", r.Request.WorkDir)
	}
	return append(args, strings.Fields(ResumePrompt)...) // ask joins its positionals with spaces
}

// staleFrom fills the fields every stale entry shows.
func staleFrom(r Record, now time.Time, reason string) StaleJob {
	return StaleJob{ID: r.ID, Task: taskOf(r), Agent: cmp.Or(r.RanAs, r.Agent), Reason: reason,
		Model: cmp.Or(r.Model, r.Invoked, r.Request.Model, "?"), Age: now.Sub(r.Time),
		Silence: now.Sub(lastSignal(r)), LastLine: lastLogLine(r.LogPath())}
}

var taskIDRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^[ \t]*TASK ID:[ \t]*(\S[^\r\n]*)`) })

// taskOf is the brief's `TASK ID:` line, else its first line.
func taskOf(r Record) string {
	if m := taskIDRe().FindStringSubmatch(r.Request.Prompt); m != nil {
		return strings.TrimSpace(m[1])
	}
	first, _, _ := strings.Cut(strings.TrimSpace(r.Request.Prompt), "\n")
	return textutil.PreviewRedacted(first, 60)
}

// collected reports a collect marker for id (written by the PostToolUse hook on an `agents result` read).
func collected(id string) bool {
	p := WatchMarker("collected", id)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// lastSignal is the newest mtime of the job's events file or log, else its start.
func lastSignal(r Record) time.Time {
	at := r.Time
	for _, p := range []string{r.EventsPath(), r.LogPath()} {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(at) {
			at = info.ModTime()
		}
	}
	return at
}

// logTailBytes bounds the log read for the last line.
const logTailBytes = 4096

// lastLogLine is the log's last non-empty line, dropped when it holds a credential shape.
func lastLogLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > logTailBytes {
		if _, err := f.Seek(-logTailBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, logTailBytes))
	if err != nil {
		return ""
	}
	line := strings.Map(func(r rune) rune {
		if r < ' ' && r != '\t' {
			return -1
		}
		return r
	}, textutil.LastLine(string(b)))
	if textutil.CredentialRE().MatchString(line) {
		return ""
	}
	return line
}

// jobSession is the session id a job can resume: its record's, the one it resumed, else one its events name.
func jobSession(r Record) string {
	if s := cmp.Or(r.SessionID, r.Request.SessionID); s != "" {
		return s
	}
	b, err := os.ReadFile(r.EventsPath())
	if err != nil {
		return ""
	}
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		var ev struct {
			SessionID string `json:"session_id"`
		}
		if bytes.Contains(line, []byte(`"session_id"`)) && json.Unmarshal(line, &ev) == nil && ev.SessionID != "" {
			return ev.SessionID
		}
	}
	return ""
}

// scanRelays finds, for each job, the subagent transcript naming it (written since the job started) and its state.
func scanRelays(recs []Record) map[string]relayState {
	out := map[string]relayState{}
	if len(recs) == 0 {
		return out
	}
	since := recs[0].Time
	for _, r := range recs {
		if r.Time.Before(since) {
			since = r.Time
		}
	}
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "*", "subagents", "agent-*.jsonl"))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil && !info.ModTime().Before(since) {
			matchRelay(out, recs, f, info.ModTime())
		}
	}
	return out
}

// matchRelay records transcript f against every job it names that started before it was last written.
func matchRelay(out map[string]relayState, recs []Record, f string, at time.Time) {
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	for _, r := range recs {
		if _, seen := out[r.ID]; !seen && !at.Before(r.Time) && bytes.Contains(b, []byte(r.ID)) {
			out[r.ID] = relayState{finished: metaFinished(f), at: at}
		}
	}
}

// metaFinished reads the transcript's sidecar agent-<id>.meta.json `finished` flag.
func metaFinished(transcript string) bool {
	b, err := os.ReadFile(strings.TrimSuffix(transcript, ".jsonl") + ".meta.json")
	if err != nil {
		return false
	}
	var m struct {
		Finished bool `json:"finished"`
	}
	return json.Unmarshal(b, &m) == nil && m.Finished
}

// WatchLines renders one stale job as lines of at most width columns: head, last log line, next command.
func WatchLines(s StaleJob, width int) []string {
	tail := " · " + s.Agent + " · " + s.Model + " · age " + ShortDur(s.Age) + " · silent " + ShortDur(s.Silence) +
		" · " + s.Reason
	head := textutil.Truncate(s.Task, max(width-len([]rune(tail)), 12)) + tail
	lines := []string{textutil.Truncate(head, width)}
	if s.LastLine != "" {
		lines = append(lines, textutil.Truncate("  last: "+s.LastLine, width))
	}
	return append(lines, wrapCommand(s.Next, width)...)
}

// wrapCommand lays argv out as `  next: ...` lines of at most width columns, continued with ` \`.
func wrapCommand(argv []string, width int) []string {
	var lines []string
	line := "  next:"
	for _, a := range argv {
		tok := textutil.QuoteSH(a)
		if len(line)+1+len(tok)+2 > width && strings.TrimSpace(line) != "next:" && line != "     " {
			lines = append(lines, line+" \\")
			line = "     "
		}
		line += " " + tok
	}
	return append(lines, line)
}

// ShortDur spells d as 45s, 12m or 1h5m.
func ShortDur(d time.Duration) string {
	switch d = max(d, 0); {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
	h := int(d.Hours())
	return strconv.Itoa(h) + "h" + strconv.Itoa(int(d.Minutes())-60*h) + "m"
}
