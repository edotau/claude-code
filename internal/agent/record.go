package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// dispatchSchema versions the dispatch record file.
const dispatchSchema = 1

var dispatchFormat = schemas.Register(schemas.Format{
	Name: "dispatch-record", Owner: "internal/agent", Kind: schemas.Record, Version: dispatchSchema,
	Glob:    func() string { return filepath.Join(paths.DispatchDir(), "*.json") },
	New:     func() any { return new(Record) },
	Clean:   "dispatch-log",
	Migrate: schemas.SameShape,
})

// Record is one `ask` dispatch: written as "running" before the run, so a killed run stays visible.
type Record struct {
	Schema         int        `json:"schema"`
	ID             string     `json:"id"`
	Time           time.Time  `json:"time"`
	Agent          string     `json:"agent"`
	Request        RunOptions `json:"request"`
	Status         string     `json:"status"` // running | ok | error
	Error          string     `json:"error,omitempty"`
	Provider       string     `json:"provider,omitempty"`
	Model          string     `json:"model,omitempty"`         // as the agent reported it; may be empty
	Invoked        string     `json:"invoked_model,omitempty"` // the model the last hop was asked to run; RunOptions keeps the original
	SessionID      string     `json:"session_id,omitempty"`
	DurationMs     int64      `json:"duration_ms,omitempty"`
	Fallback       string     `json:"fallback,omitempty"` // the original agent's name, when it ran instead
	FallbackReason string     `json:"fallback_reason,omitempty"`
	RanAs          string     `json:"ran_as,omitempty"`          // the agent that answered, when a fallback dispatch did
	Repo           string     `json:"repo,omitempty"`            // main checkout root the job belongs to
	Route          string     `json:"route,omitempty"`           // --route at launch (databricks:qa), so a running job shows where it is going
	PID            int        `json:"pid,omitempty"`             // pid of the detached `claude-code agents ask --job` child
	PIDStart       string     `json:"pid_start,omitempty"`       // proc.StartTime of PID at spawn: tells a reused pid apart
	Log            string     `json:"log,omitempty"`             // absolute path of the job's combined stdout+stderr log
	Progress       string     `json:"progress,omitempty"`        // multi-part review job: "part 2/3"
	Owner          int        `json:"owner,omitempty"`           // launching Claude Code session pid (inflight.SessionOwner)
	Host           string     `json:"host,omitempty"`            // ask --host: the ssh host the dispatch ran on
	RemoteBin      string     `json:"remote_bin,omitempty"`      // ask --host: the remote claude-code binary
	FallbackPolicy string     `json:"fallback_policy,omitempty"` // the --fallback mode asked for (all|agents|none)
	Live           bool       `json:"live,omitempty"`            // ask --live: a claude session held open on <id>.sock
	TaskID         string     `json:"task_id,omitempty"`         // ask --batch: the task line's id
	BatchID        string     `json:"batch_id,omitempty"`        // ask --batch: the id shared by every task of the run
	PlanTask       string     `json:"plan_task,omitempty"`       // ask --batch: the task line's optional plan_task

	Widened []string `json:"permissions_widened,omitempty"` // what the request grants beyond the agent's default permissions
}

// Alive reports whether this dispatch is still running, by the shared inflight.DispatchAlive rule.
func (r Record) Alive() bool { return inflight.DispatchAlive(r.Status, r.PID, r.Time) }

// LogPath is the dispatch's log file: Log when set, else <id>.log beside the record.
func (r Record) LogPath() string {
	return cmp.Or(r.Log, filepath.Join(paths.DispatchDir(), r.ID+".log"))
}

// EventsPath is the dispatch's NDJSON event file, <id>.events.jsonl beside the record.
func (r Record) EventsPath() string {
	return filepath.Join(paths.DispatchDir(), r.ID+paths.DispatchEventsSuffix)
}

// dispatchKeep bounds the log after pruning once the margin is exceeded.
var dispatchKeep = 200

// DispatchKeep is how many newest dispatch records pruning keeps; `claude-code clean` never sweeps them.
func DispatchKeep() int { return dispatchKeep }

const dispatchPruneMargin = 20

// NewRecord stamps an id that sorts by time and names the agent: 20261016T210512.123-codex.
func NewRecord(name string, req RunOptions, now time.Time) Record {
	req.EnvNames = envNames(req.Env)
	widened, _ := widenedChecked(name, req.Permissions) // a bad default fails the run itself
	return Record{Schema: dispatchSchema, ID: recordID(now, name), Time: now.UTC(), Agent: name, Request: req, Status: "running",
		Widened: widened, Route: os.Getenv(router.EnvRoute)}
}

// widenedChecked is what p grants beyond name's default permissions; an unreadable default is returned, not hidden.
func widenedChecked(name string, p Permissions) ([]string, error) {
	def, err := DefaultPermissions(name)
	_, widened := resolvePermissions(def, p)
	return widened, err
}

// ClaimRecord is NewRecord with its id reserved on disk: the allocator for every new dispatch (ask, batch, job).
func ClaimRecord(agent string, req RunOptions, now time.Time) Record {
	return claimRecord(NewRecord(agent, req, now))
}

// claimRecord gives rec an id no other dispatch, batch or process holds: it links the complete "running" record to
// <id>.json (atomic, EEXIST if taken), bumping the millisecond stamp on a clash, so a claim is never an empty file.
// Failures leave rec unclaimed (fail-open, like Save): the log is for replay, never a gate on the run.
func claimRecord(rec Record) Record {
	dir := paths.DispatchDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return rec
	}
	for {
		body, err := rec.encode()
		if err != nil {
			return rec
		}
		tmp, err := os.CreateTemp(dir, ".claim-*")
		if err != nil {
			return rec
		}
		_, werr := tmp.Write(body)
		cerr := tmp.Close()
		err = cmp.Or(werr, cerr)
		if err == nil {
			err = os.Link(tmp.Name(), filepath.Join(dir, rec.ID+".json"))
		}
		_ = os.Remove(tmp.Name())
		if !errors.Is(err, os.ErrExist) {
			return rec
		}
		rec.Time = rec.Time.Add(time.Millisecond)
		rec.ID = recordID(rec.Time, rec.Agent)
	}
}

// recordID is the id NewRecord stamps for agent at now.
func recordID(now time.Time, agent string) string {
	return now.UTC().Format("20060102T150405.000") + "-" + agent
}

// encode is the record's JSON, schema stamped.
func (r Record) encode() ([]byte, error) {
	r.Schema = dispatchSchema
	return json.MarshalIndent(r, "", "  ")
}

// Save writes the record (0600) and prunes past dispatchKeep. The prompt is kept verbatim so a retry is exact.
func (r Record) Save() error {
	r.stampOwnStart()
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := r.encode()
	if err != nil {
		return err
	}
	if err := paths.AtomicWrite(filepath.Join(dir, r.ID+".json"), b, 0o600); err != nil {
		return err
	}
	pruneDispatches(dir)
	return nil
}

// stampOwnStart records this process's start time when it saves its own pid and none is on file (a --job child).
func (r *Record) stampOwnStart() {
	if r.PID == os.Getpid() && r.PIDStart == "" {
		r.PIDStart, _ = proc.StartTime(r.PID) // "" on failure: Cancel then refuses to signal this pid
	}
}

// Elapsed is the run time as text; "" for a record with no duration (running, or written before duration_ms).
func (r Record) Elapsed() string {
	if r.DurationMs <= 0 {
		return ""
	}
	return (time.Duration(r.DurationMs) * time.Millisecond).String()
}

// LogLine renders r as one `ask --log` row, plus a fallback line; the prompt preview is scrubbed of credential shapes.
func (r Record) LogLine() string {
	tools := ""
	if r.Request.Tools {
		tools = " tools"
	}
	agentCol := r.Agent
	if r.Fallback != "" {
		agentCol = r.Fallback + "→" + cmp.Or(r.RanAs, "?")
	}
	line := fmt.Sprintf("%-28s %-7s %-8s %-9s%s  %s\n", r.ID, r.Status, agentCol, r.Elapsed(), tools, textutil.PreviewRedacted(r.Request.Prompt, 60))
	if r.FallbackReason != "" {
		line += fmt.Sprintf("%-28s   fallback: %s\n", "", r.FallbackReason)
	}
	return line
}

// Finish records the outcome of the run.
func (r *Record) Finish(res wire.Result, err error, took time.Duration) {
	r.Status, r.Provider, r.Model, r.SessionID = "ok", res.Provider, res.Model, res.SessionID
	r.Invoked = res.Invoked
	r.Fallback, r.FallbackReason = res.Fallback, textutil.RedactSecrets(res.FallbackReason)
	if res.Fallback != "" {
		r.RanAs = res.Agent
		if res.Agent != "" && res.Agent != r.Agent { // widening is judged against the agent that answered
			if w, err := widenedChecked(res.Agent, r.Request.Permissions); err == nil { // keep the original on a bad default
				r.Widened = w
			}
		}
	}
	r.DurationMs = took.Milliseconds()
	if err != nil {
		r.Status, r.Error = "error", textutil.RedactSecrets(err.Error())
	}
}

func pruneDispatches(dir string) {
	names, err := recordNames(dir)
	if err != nil || len(names) <= dispatchKeep+dispatchPruneMargin { // best-effort: a failed listing skips this prune
		return
	}
	excess := len(names) - dispatchKeep
	for _, name := range names {
		if excess <= 0 {
			return
		}
		if activeRecord(filepath.Join(dir, name)) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".json")+".log")) // ignore not-exist
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".json")+paths.DispatchEventsSuffix))
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".json")+wire.LiveSockSuffix))
		excess--
	}
}

// activeRecord keeps a live dispatch (see Record.Alive) out of the prune.
func activeRecord(path string) bool {
	r, err := readRecord(path)
	if err != nil {
		return errors.Is(err, schemas.ErrNewer) // a newer build's record is never pruned blind
	}
	return r.Alive()
}

// readRecord reads and unmarshals one record file by its full path.
func readRecord(path string) (Record, error) {
	var r Record
	b, err := os.ReadFile(path)
	if err == nil {
		_, err = schemas.Decode(dispatchFormat, b, &r)
	}
	return r, err
}

// recordNames lists record files oldest first (ids sort by time); a missing dir is no records.
func recordNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("dispatch dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// LoadRecord reads one record by exact id or unique id prefix.
func LoadRecord(id string) (Record, error) {
	names, err := recordNames(paths.DispatchDir())
	if err != nil {
		return Record{}, fmt.Errorf("dispatch %q: %w", id, err)
	}
	var match []string
	for _, n := range names {
		if n == id+".json" {
			return readRecord(filepath.Join(paths.DispatchDir(), n))
		}
		if strings.HasPrefix(n, id) {
			match = append(match, n)
		}
	}
	if len(match) != 1 {
		return Record{}, fmt.Errorf("dispatch %q: %d matches (see claude-code agents ask --log)", id, len(match))
	}
	return readRecord(filepath.Join(paths.DispatchDir(), match[0]))
}

// AllRecords is RecentRecords' n for "every record": the log itself is capped by the dispatch keep.
const AllRecords = 1 << 20

// RecentRecords returns up to n records, newest first; an unreadable dispatch dir lists none.
func RecentRecords(n int) []Record {
	out, _ := ReadRecent(n)
	return out
}

// ReadRecent is RecentRecords that also returns an unreadable dispatch dir's error.
func ReadRecent(n int) ([]Record, error) {
	dir := paths.DispatchDir()
	names, err := recordNames(dir)
	var out []Record
	for i := len(names) - 1; i >= 0 && len(out) < n; i-- {
		if r, err := readRecord(filepath.Join(dir, names[i])); err == nil {
			out = append(out, r)
		}
	}
	return out, err
}

// RunRecordedOpts tunes RunRecorded for the caller's stdout contract.
type RunRecordedOpts struct {
	Job      bool         // a --job child: stdout is the log, so out is dropped and the log keeps events + stderr only
	AnswerTo io.Writer    // written the final answer once the log is flushed (a job's `<agent> result` reads it)
	Notify   func(string) // receives non-fatal bookkeeping failures (the record not saving)
}

// runRecorded runs one recorded dispatch (log, event file, start, run, Finish/Save, result): one pair per id, not per dispatch.
func runRecorded(ctx context.Context, rec *Record, agentName string, out wire.Sink, o RunRecordedOpts,
	run func(context.Context, wire.Sink) (wire.Result, error)) (wire.Result, error) {
	logSink := wire.NewLogSink(rec.LogPath())
	defer logSink.Close() // idempotent: covers a panic in Run
	ctx = wire.WithLog(ctx, logSink)
	ctx = withDispatchID(ctx, rec.ID)
	if o.Job {
		ctx, out = wire.WithLogEvents(ctx, logSink), wire.Discard
	}
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	sink := wire.Multi(out, events)
	begin := time.Now()
	res := wire.Result{Agent: agentName, Status: wire.StatusError}
	err := sink.Emit(wire.Event{Type: wire.EventStart, Agent: agentName, DispatchID: rec.ID, Status: "running"})
	if err == nil {
		res, err = run(ctx, sink)
		logSink.Close() // flush before writing the answer, so tail -f never misses the last line
		if o.AnswerTo != nil && res.Answer != "" {
			if _, werr := io.WriteString(o.AnswerTo, res.Answer); werr != nil && err == nil {
				err = fmt.Errorf("write final answer: %w", werr)
			}
		}
		// the result event lands before the record leaves running, so a tail follower never exits without it
		if rerr := sink.Emit(wire.ResultEvent(wire.NewEnvelope(res, err, rec.ID))); rerr != nil && err == nil {
			err = rerr
		}
	}
	logSink.Close()
	rec.Finish(res, err, time.Since(begin))
	if serr := rec.Save(); serr != nil && o.Notify != nil {
		o.Notify(fmt.Sprintf("dispatch %s record not saved, it stays at running: %v", rec.ID, serr))
	}
	return res, err
}

// ResultEnvelope is the result event's envelope, or one rebuilt from rec when no result event can be read.
func (rec Record) ResultEnvelope(errw io.Writer) wire.Envelope {
	if f, err := os.Open(rec.EventsPath()); err == nil {
		defer f.Close()
		var env *wire.Envelope
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var ev wire.Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == wire.EventResult && ev.Envelope != nil {
				env = ev.Envelope
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(errw, "agents result: %s: events unreadable, envelope may be incomplete: %v\n", rec.ID, err)
		}
		if env != nil {
			return *env
		}
	}
	var runErr error
	if rec.Status == "error" {
		runErr = fmt.Errorf("%s", rec.Error)
	}
	res := wire.Result{Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model, Invoked: rec.Invoked,
		SessionID: rec.SessionID, DurationMs: rec.DurationMs, Fallback: rec.Fallback, FallbackReason: rec.FallbackReason}
	if log, err := os.ReadFile(rec.LogPath()); err == nil {
		res.Answer = string(log)
	}
	return wire.NewEnvelope(res, runErr, rec.ID)
}
