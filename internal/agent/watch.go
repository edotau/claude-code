// watch.go classifies a session's dispatch jobs as ok or stale for `agents watch` and the hook watchdog.
package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/parallel"
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
	WatchQueueStuck  = "queue-stuck" // admission exceeded its budget or its daemon disappeared
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
	collectHorizon      = 2 * time.Hour    // older finished jobs predate any collect marker: never flagged
	quickFailure        = 10 * time.Second // an error this soon after start was shown to its caller
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
	Parent    string        `json:"parent,omitempty"`
	Depth     int           `json:"depth,omitempty"`
	Age       time.Duration `json:"age_ns"`
	Silence   time.Duration `json:"silence_ns"`
	LastLine  string        `json:"last_line,omitempty"`
	Queue     string        `json:"queue,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Next      []string      `json:"next"` // argv of the next command, starting with claude-code
}

// Resumable is a dead job the watchdog may resume: it has a session to continue.
func (s StaleJob) Resumable() bool { return s.Reason == WatchDead && s.SessionID != "" }

// WatchReport is one pass: the stale jobs, and how many more may still turn stale.
type WatchReport struct {
	Stale   []StaleJob  `json:"stale"`
	Pending int         `json:"pending"`
	Running []Record    `json:"-"` // the healthy pending jobs, for the hook's heartbeat
	Queued  []QueuedJob `json:"queued,omitempty"`
}

// QueuedJob is a daemon-observed admission wait; Ahead is -1 when only the record is available.
type QueuedJob struct {
	ID    string        `json:"id"`
	Pool  string        `json:"pool"`
	Wait  time.Duration `json:"wait_ns"`
	Ahead int           `json:"ahead"`
}

func (q QueuedJob) Line() string {
	line := "queued on " + textutil.PreviewRedacted(q.Pool, 60) + " " + ShortDur(q.Wait)
	if q.Ahead >= 0 {
		line += fmt.Sprintf(", %d ahead", q.Ahead)
	}
	return line
}

// QueueWatch is the serve adapter's bounded observation; Gone requires positive evidence of an absent daemon.
type QueueWatch struct {
	Jobs  map[string]QueuedJob
	Limit time.Duration
	Gone  bool
}

// WatchQueue is installed by serve to avoid an agent-to-serve import cycle; unqueued watches never call it.
var WatchQueue func(time.Time) QueueWatch

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

// ResumedAs is the job id the watchdog resumed id as; an empty marker is a claim whose launch never finished.
func ResumedAs(id string) (string, bool) {
	p := WatchMarker("resumed", id)
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	next := strings.TrimSpace(string(b))
	return next, err == nil && next != ""
}

// Watch classifies owner's jobs (and the jobs the watchdog resumed for them) at now.
func Watch(owner int, now time.Time, lim WatchLimits) (WatchReport, error) {
	if owner <= 1 {
		_, err := recordNames(paths.DispatchDir()) // no jobs to classify; ReadRecent's only error is this listing's
		return WatchReport{}, err
	}
	recs, err := ownerRecords(owner, now)
	return classifyJobs(ownedJobs(recs, owner), now, lim, scanRelays), err
}

// ownerRecords is ReadRecent(AllRecords) from owner's first record on, a bound cached per owner process start.
func ownerRecords(owner int, now time.Time) ([]Record, error) {
	dir := paths.DispatchDir()
	names, err := recordNames(dir)
	token, terr := proc.StartTime(owner)
	marker := filepath.Join(WatchDir(), "owner-"+strconv.Itoa(owner))
	first, cached := "", false
	if b, rerr := os.ReadFile(marker); terr == nil && rerr == nil {
		var tok string
		tok, first, cached = strings.Cut(string(b), "\n")
		cached = cached && tok == token
	}
	if !cached {
		first = ""
	}
	i, _ := slices.BinarySearch(names, first) // ids sort by time and a later record has a later id
	recs := readNames(dir, names[i:], "")
	for i, r := range recs {
		recs[i] = reapLost(r)
	}
	if terr == nil && !cached && os.MkdirAll(WatchDir(), 0o700) == nil {
		_ = os.WriteFile(marker, []byte(token+"\n"+firstOwned(recs, owner, now)), 0o600) // a lost write rescans next time
	}
	return recs, err
}

// firstOwned is the oldest id owner launched in recs (newest first), else an id a minute before now.
func firstOwned(recs []Record, owner int, now time.Time) string {
	for _, r := range slices.Backward(recs) {
		if r.Owner == owner {
			return r.ID
		}
	}
	return recordID(now.Add(-time.Minute), "") // a record stamped just before the listing may land after it
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
	queue := watchQueue(recs, now)
	rel := relays(relayCandidates(recs, now))
	for _, r := range recs {
		s, pending := classifyJob(r, now, lim, rel[r.ID])
		q, queued := queuedWatch(r, now, queue)
		if queued && r.Status == "running" && pending {
			s, pending = StaleJob{}, true
			if queue.Gone || q.Wait > queue.Limit {
				s, pending = staleFrom(r, now, WatchQueueStuck), false
				s.Queue = q.Line() + "; admission wait exceeded"
				if queue.Gone {
					s.Queue = q.Line() + "; daemon gone"
				}
				s.Next = []string{"claude-code", "agents", "result", "-f", r.ID} // alive: a retry would run it twice
			} else {
				rep.Queued = append(rep.Queued, q)
			}
		}
		if s.Reason != "" {
			stale = append(stale, s)
		}
		if pending {
			rep.Pending++
			if r.Status == "running" && !queued {
				rep.Running = append(rep.Running, r)
			}
		}
	}
	rep.Stale = stale
	return rep
}

func watchQueue(recs []Record, now time.Time) QueueWatch {
	if WatchQueue != nil {
		for _, r := range recs {
			if pool, _ := r.admissionQueue(); pool != "" {
				return WatchQueue(now)
			}
		}
	}
	return QueueWatch{}
}

func queuedWatch(r Record, now time.Time, queue QueueWatch) (QueuedJob, bool) {
	pool, since := r.admissionQueue()
	if pool == "" || queue.Limit <= 0 {
		return QueuedJob{}, false
	}
	if q, ok := queue.Jobs[r.ID]; ok {
		q.Wait = max(q.Wait, now.Sub(since))
		return q, true
	}
	return QueuedJob{ID: r.ID, Pool: pool, Wait: now.Sub(since), Ahead: -1}, true
}

// relayCandidates keeps running jobs and ended jobs not yet settled by age or collection.
func relayCandidates(recs []Record, now time.Time) []Record {
	return slices.DeleteFunc(slices.Clone(recs), func(r Record) bool {
		if r.Status == "running" {
			return false
		}
		end := r.Time.Add(time.Duration(r.DurationMs) * time.Millisecond)
		return !r.Background || r.Failed() && end.Sub(r.Time) < quickFailure || now.Sub(end) > collectHorizon || collected(r.ID)
	})
}

// classifyJob is one record's verdict: a stale entry (empty Reason when ok) and whether it may still turn stale.
func classifyJob(r Record, now time.Time, lim WatchLimits, rel relayState) (StaleJob, bool) {
	if r.Status == "running" {
		return classifyRunning(r, now, lim, rel)
	}
	return classifyEnded(r, now, rel)
}

// classifyEnded checks a finished record: a background job whose result nobody collected.
func classifyEnded(r Record, now time.Time, rel relayState) (StaleJob, bool) {
	end := r.Time.Add(time.Duration(r.DurationMs) * time.Millisecond)
	if !r.Background || r.Failed() && end.Sub(r.Time) < quickFailure {
		return StaleJob{}, false
	}
	if settled(r.ID, now, end, rel) {
		return StaleJob{}, false
	}
	if now.Sub(end) <= uncollectedAfter {
		return StaleJob{}, true
	}
	s := staleFrom(r, now, WatchUncollected)
	s.Next = []string{"claude-code", "agents", "result", r.ID}
	if r.Failed() && recoverable(r) {
		s.Next = recoverArgs(r)
	}
	return s, false
}

// settled reports whether an ended job is past the horizon, collected, or relayed after it ended.
func settled(id string, now, end time.Time, rel relayState) bool {
	return now.Sub(end) > collectHorizon || collected(id) || rel.finished && !rel.at.Before(end)
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
		switch {
		case s.SessionID != "":
			s.Next = ResumeArgs(r, s.SessionID)
		case recoverable(r):
			s.Next = recoverArgs(r)
		}
		return s, false
	}
	if r.QueueLine(now) != "" {
		return StaleJob{}, true
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

// recoverable reports whether r's event log holds text or tool events, so `ask --retry --recover` has a log to resume.
func recoverable(r Record) bool {
	_, err := recoveryLog(r.EventsPath())
	return err == nil
}

// recoverArgs is the suggested (never auto-run) `ask --retry --recover` argv for r.
func recoverArgs(r Record) []string {
	return []string{"claude-code", "agents", "ask", "--retry", r.ID, "--recover"}
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
	return append(append(args, "--"), strings.Fields(ResumePrompt)...) // after "--" no prompt word is read as a flag
}

// staleFrom fills the fields every stale entry shows.
func staleFrom(r Record, now time.Time, reason string) StaleJob {
	lastAt, _ := LastSignal(r)
	return StaleJob{ID: r.ID, Task: taskOf(r), Agent: cmp.Or(r.RanAs, r.Agent), Reason: reason,
		Parent: r.Parent, Depth: r.Depth,
		Model: cmp.Or(r.Model, r.Invoked, r.Request.Model, "?"), Age: now.Sub(r.Time),
		Silence: now.Sub(lastAt), LastLine: lastLogLine(r.LogPath())}
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

// LastSignal is the newest mtime of the dispatch's events file or log, else its start, and which one it was.
func LastSignal(r Record) (time.Time, string) {
	at, source := r.Time, "record"
	for _, c := range []struct{ path, name string }{{r.EventsPath(), "events"}, {r.LogPath(), "log"}} {
		info, err := os.Stat(c.path)
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if c.name == "log" { // heartbeat lines touch the log: the quiet they report is not output
			mt = mt.Add(-time.Duration(heartbeatTail(c.path)) * heartbeatEvery)
		}
		if mt.After(at) {
			at, source = mt, c.name
		}
	}
	return at, source
}

// heartbeatTail counts the heartbeat lines that end the log, within its last logTailBytes.
func heartbeatTail(path string) int {
	b, err := tailBytes(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, l := range slices.Backward(strings.Split(strings.TrimRight(string(b), "\n"), "\n")) {
		if !strings.HasPrefix(l, "▸ heartbeat:") {
			break
		}
		n++
	}
	return n
}

// tailBytes is the last logTailBytes of the file at path.
func tailBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > logTailBytes {
		if _, err := f.Seek(-logTailBytes, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, logTailBytes))
}

// logTailBytes bounds the log read for the last line.
const logTailBytes = 4096

// lastLogLine is the log's last non-empty line, dropped when it holds a credential shape.
func lastLogLine(path string) string {
	b, err := tailBytes(path)
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
	ids := map[string]bool{}
	for _, r := range recs {
		ids[r.ID] = true
	}
	files := relayTranscripts(filepath.Join(paths.ConfigDir(), "projects"))
	since := oldestUnmatched(recs, out)
	for _, f := range files {
		if len(out) == len(ids) && !anyFinished(out) {
			break // every job maps to a live relay: later transcripts cannot change an entry
		}
		if info, err := os.Stat(f); err == nil && !info.ModTime().Before(since) {
			matchRelay(out, recs, f, info.ModTime())
			since = oldestUnmatched(recs, out)
		}
	}
	return out
}

// anyFinished reports whether some job is matched only to a finished transcript, which a live one may still replace.
func anyFinished(out map[string]relayState) bool {
	for _, st := range out {
		if st.finished {
			return true
		}
	}
	return false
}

// oldestUnmatched is the earliest start among jobs with no live relay in out: no older transcript can name them.
func oldestUnmatched(recs []Record, out map[string]relayState) time.Time {
	var since time.Time
	first := true
	for _, r := range recs {
		if st, seen := out[r.ID]; (!seen || st.finished) && (first || r.Time.Before(since)) {
			since, first = r.Time, false
		}
	}
	return since
}

// relayChunk is the read size of matchRelay: memory is bounded by it plus one id, not by the transcript size.
const relayChunk = 64 << 10

// relayTranscripts lists <projects>/*/*/subagents/agent-*.jsonl in glob order, one project dir per worker.
func relayTranscripts(projects string) []string {
	dirs, _ := os.ReadDir(projects)
	perDir, _ := parallel.Map(context.Background(), dirs, 8, func(_ context.Context, d os.DirEntry) ([]string, error) {
		var files []string
		sessions, _ := os.ReadDir(filepath.Join(projects, d.Name()))
		for _, sess := range sessions {
			sub := filepath.Join(projects, d.Name(), sess.Name(), "subagents")
			names, _ := os.ReadDir(sub)
			for _, n := range names {
				if ok, _ := filepath.Match("agent-*.jsonl", n.Name()); ok {
					files = append(files, filepath.Join(sub, n.Name()))
				}
			}
		}
		return files, nil
	})
	return slices.Concat(perDir...)
}

// matchRelay records transcript f against the jobs it names; a live transcript replaces a finished one.
func matchRelay(out map[string]relayState, recs []Record, f string, at time.Time) {
	var want []Record
	keep := 0
	for _, r := range recs {
		if st, seen := out[r.ID]; (!seen || st.finished) && !at.Before(r.Time) {
			want = append(want, r)
			keep = max(keep, len(r.ID)-1)
		}
	}
	if len(want) == 0 {
		return
	}
	fd, err := os.Open(f)
	if err != nil {
		return
	}
	defer fd.Close()
	buf := make([]byte, relayChunk+keep)
	carried := 0
	for len(want) > 0 {
		n, err := fd.Read(buf[carried : carried+relayChunk])
		data := buf[:carried+n]
		want = slices.DeleteFunc(want, func(r Record) bool {
			if !bytes.Contains(data, []byte(r.ID)) {
				return false
			}
			out[r.ID] = relayState{finished: metaFinished(f), at: at}
			return true
		})
		if err != nil {
			return
		}
		carried = min(len(data), keep)
		copy(buf, data[len(data)-carried:])
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
	head := textutil.TruncateCells(s.Task, max(width-textutil.DisplayWidth(tail), 12), "…") + tail
	lines := []string{textutil.TruncateCells(head, width, "…")}
	if s.Queue != "" {
		lines = []string{textutil.TruncateCells(s.ID+" · "+s.Reason, width, "…")}
		for detail := range strings.SplitSeq(s.Queue, "; ") {
			lines = append(lines, textutil.TruncateCells("  "+detail, width, "…"))
		}
	}
	if s.LastLine != "" && s.Queue == "" {
		lines = append(lines, textutil.TruncateCells("  last: "+s.LastLine, width, "…"))
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
