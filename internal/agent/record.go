package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
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

// FailKind values: what a failed record blames, for the routing fit table.
const (
	FailAvailability = "availability"
	FailQuality      = "quality"
)

// Record is one `ask` dispatch: written as "running" before the run, so a killed run stays visible.
type Record struct {
	Schema         int        `json:"schema"`
	ID             string     `json:"id"`
	Time           time.Time  `json:"time"`
	Agent          string     `json:"agent"`
	Request        RunOptions `json:"request"`
	Status         string     `json:"status"` // running | ok | error
	Error          string     `json:"error,omitempty"`
	FailKind       string     `json:"fail_kind,omitempty"` // availability | quality; empty when the run succeeded
	Gates          string     `json:"gates,omitempty"`     // pass | fail | none (empty reads as none): set by SetGates
	Provider       string     `json:"provider,omitempty"`
	Model          string     `json:"model,omitempty"`          // as the agent reported it; may be empty
	Invoked        string     `json:"invoked_model,omitempty"`  // the model the last hop was asked to run; RunOptions keeps the original
	Reported       string     `json:"reported_model,omitempty"` // the id the final attempt's vendor stream named; "" when none
	SessionID      string     `json:"session_id,omitempty"`
	DurationMs     int64      `json:"duration_ms,omitempty"`
	Fallback       string     `json:"fallback,omitempty"` // the original agent's name, when it ran instead
	FallbackReason string     `json:"fallback_reason,omitempty"`
	RanAs          string     `json:"ran_as,omitempty"`          // the agent that answered, when a fallback dispatch did
	Repo           string     `json:"repo,omitempty"`            // main checkout root the job belongs to
	RanRoute       string     `json:"ran_route,omitempty"`       // provider:profile of the row that answered, as the ran-on line names it
	Attempts       []wire.Hop `json:"attempts,omitempty"`        // every hop of a re-picked or moved run; absent for a one-hop run
	Route          string     `json:"route,omitempty"`           // overlay confinement at launch (databricks:qa), so a running job shows where it is going
	PID            int        `json:"pid,omitempty"`             // pid of the detached `claude-code agents ask --job` child
	PIDStart       string     `json:"pid_start,omitempty"`       // proc.StartTime of PID at spawn: tells a reused pid apart
	Background     bool       `json:"background,omitempty"`      // set only by the `ask --background` spawn
	Log            string     `json:"log,omitempty"`             // absolute path of the job's combined stdout+stderr log
	Progress       string     `json:"progress,omitempty"`        // multi-part review job: "part 2/3"
	Owner          int        `json:"owner,omitempty"`           // launching Claude Code session pid (inflight.SessionOwner)
	Host           string     `json:"host,omitempty"`            // ask --host: the ssh host the dispatch ran on
	FallbackPolicy string     `json:"fallback_policy,omitempty"` // the --fallback mode asked for (all|agents|none)
	Live           bool       `json:"live,omitempty"`            // ask --live: a claude session held open on <id>.sock
	TaskID         string     `json:"task_id,omitempty"`         // ask --batch: the task line's id
	BatchID        string     `json:"batch_id,omitempty"`        // ask --batch: the id shared by every task of the run
	PlanTask       string     `json:"plan_task,omitempty"`       // ask --batch: the task line's optional plan_task
	Preflight      *Preflight `json:"preflight,omitempty"`       // what the preflight failed or skipped, and what it chose
	Wrote          *[]string  `json:"wrote,omitempty"`           // files the run changed, absolute; nil: not observed
	Tree           string     `json:"tree,omitempty"`            // git top of the working tree the run started in
	TreeChanged    *[]string  `json:"tree_changed,omitempty"`    // files changed in a tree another dispatch shared; Wrote is then nil

	Parent string `json:"parent,omitempty"`
	Depth  int    `json:"depth,omitempty"`
	// MaxDepth is the depth cap this dispatch ran under (0 = none); nil is a legacy record, read as the default cap.
	MaxDepth *int        `json:"max_depth,omitempty"`
	Usage    *wire.Usage `json:"usage,omitempty"`

	Widened []string `json:"permissions_widened,omitempty"` // what the request grants beyond the agent's default permissions

	// PreDirtyTouched: files dirty before the run that differ after it; advisory, may be the dispatcher's own work.
	PreDirtyTouched *[]string `json:"predirty_touched,omitempty"`
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

const DefaultMaxDispatchDepth = 3

const maxDispatchDepthEnv = "HARNESS_DISPATCH_MAX_DEPTH"

// NewRecord stamps an id that sorts by time and names the agent: 20261016T210512.123-codex.
func NewRecord(name string, req RunOptions, now time.Time) Record {
	req.EnvNames = envNames(req.Env)
	if req.Values != nil {
		req.Values = req.Values.Merge(nil)
	}
	widened, _ := widenedChecked(name, req.Permissions) // a bad default fails the run itself
	ctx := valuesContext(context.Background(), req.Values)
	parent := proc.Value(ctx, proc.DispatchIDEnv)
	depth, maxDepth := lineage(ctx, parent)
	provider, profile, _ := router.CoworkerContext(ctx)
	return Record{Schema: dispatchSchema, ID: recordID(now, name), Time: now.UTC(), Agent: name, Request: req, Status: "running",
		Parent: parent, Depth: depth, MaxDepth: &maxDepth, Widened: widened, Route: router.JoinCoworker(provider, profile)}
}

// lineage is the depth and cap from the parent's record; the env may tighten both, never loosen (cap 0 = none).
func lineage(ctx context.Context, parent string) (depth, maxDepth int) {
	env, set := envDepthCap(ctx)
	if parent == "" {
		return 0, env
	}
	inherited, _ := strconv.Atoi(proc.Value(ctx, proc.DispatchDepthEnv))
	depth, maxDepth = max(inherited, 0)+1, DefaultMaxDispatchDepth
	if pr, ok := parentRecord(parent); !ok { // a reaped or stale parent degrades to the default cap, never uncapped
		fmt.Fprintf(NoticeOut, "claude-code: parent dispatch %s has no readable record; default depth cap applies\n", parent)
	} else if depth = max(depth, pr.Depth+1); pr.MaxDepth != nil {
		maxDepth = *pr.MaxDepth
	}
	if set && env > 0 && (maxDepth == 0 || env < maxDepth) {
		maxDepth = env
	}
	return depth, maxDepth
}

// envDepthCap is HARNESS_DISPATCH_MAX_DEPTH; unset, invalid or negative is the default and not set.
func envDepthCap(ctx context.Context) (int, bool) {
	n, err := strconv.Atoi(proc.Value(ctx, maxDispatchDepthEnv))
	if err != nil || n < 0 {
		return DefaultMaxDispatchDepth, false
	}
	return n, true
}

// parentRecord is the dispatch record named exactly id, read from disk rather than trusted from the caller's env.
func parentRecord(id string) (Record, bool) {
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return Record{}, false
	}
	rec, err := readRecord(filepath.Join(paths.DispatchDir(), id+".json"))
	return rec, err == nil
}

// maxAncestorWalk bounds ancestorPolicy; past it only the rules collected so far and the env policy apply.
const maxAncestorWalk = 32

// ancestorPolicy unions the deny and ask rules, plan mode and read-only of parent and every recorded ancestor.
func ancestorPolicy(parent string) (p Permissions, readOnly bool) {
	seen := map[string]bool{}
	for id := parent; id != "" && !seen[id] && len(seen) < maxAncestorWalk; {
		seen[id] = true
		rec, ok := parentRecord(id)
		if !ok {
			break
		}
		req := rec.Request
		p.Deny, p.Ask = union(p.Deny, req.Permissions.Deny), union(p.Ask, req.Permissions.Ask)
		if req.Permissions.Mode == "plan" {
			p.Mode = "plan"
		}
		readOnly = readOnly || req.ReadOnly
		id = rec.Parent
	}
	return p, readOnly
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

// DispatchClaimEnv names the private, exclusive-create ID file used by a hook launch.
const DispatchClaimEnv = "CLAUDE_CODE_DISPATCH_CLAIM"

func ClaimDispatchID(path, id string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// claimRecord gives rec an unheld id by linking the running record to <id>.json (EEXIST bumps the stamp); fails open.
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

// ErrJobNotFresh marks a --job record that is finished or already claimed by a live runner.
var ErrJobNotFresh = errors.New("not a fresh background job")

// ClaimJob marks a fresh running job record as this process's under the record's lock; a loser gets ErrJobNotFresh.
func ClaimJob(id string) (Record, error) {
	r, err := LoadRecord(id)
	if err != nil {
		return r, err
	}
	path := filepath.Join(paths.DispatchDir(), r.ID+".json")
	err = paths.WithFileLock(path, func() error {
		var lerr error
		if r, lerr = readRecord(path); lerr != nil {
			return lerr
		}
		if r.Status != "running" || (r.PID != 0 && r.Alive()) { // a dead pid is a runner that died mid-claim
			return fmt.Errorf("--job %s: record is %s (pid %d), %w", r.ID, r.Status, r.PID, ErrJobNotFresh)
		}
		r.PID, r.PIDStart = os.Getpid(), ""
		return r.Save()
	})
	return r, err
}

// RepairDead applies fix to id's record under its lock if it is still a dead running one; false when it is not.
func RepairDead(id string, fix func(Record) Record) (Record, bool, error) {
	return updateRunning(id, true, fix)
}

// UpdateRunning applies fix to id's record under its lock if it is still running, live or dead; false when it is not.
func UpdateRunning(id string, fix func(Record) Record) (Record, bool, error) {
	return updateRunning(id, false, fix)
}

func updateRunning(id string, deadOnly bool, fix func(Record) Record) (Record, bool, error) {
	return updateRecord(id, func(r Record) (Record, bool) {
		if r.Status != "running" || (deadOnly && r.Alive()) {
			return r, false
		}
		return fix(r), true
	})
}

func updateRecord(id string, fix func(Record) (Record, bool)) (Record, bool, error) {
	path := filepath.Join(paths.DispatchDir(), id+".json")
	var r Record
	var applied bool
	err := paths.WithFileLock(path, func() error {
		var lerr error
		if r, lerr = readRecord(path); lerr != nil {
			return lerr
		}
		r, applied = fix(r)
		if !applied {
			return nil
		}
		return r.Save()
	})
	return r, applied, err
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
	path := filepath.Join(dir, r.ID+".json")
	_, statErr := os.Lstat(path)
	if err := paths.AtomicWrite(path, b, 0o600); err != nil {
		return err
	}
	if errors.Is(statErr, fs.ErrNotExist) { // only a new record can push the dir over the cap
		pruneDispatches(dir)
	}
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

// LogLine renders r as one `agents status` row plus a fallback line; the prompt preview is scrubbed of credentials.
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
	r.FailKind = ""
	r.Status, r.Provider, r.Model, r.SessionID = "ok", res.Provider, res.Model, res.SessionID
	r.Invoked, r.Reported = res.Invoked, res.Reported
	r.Usage = nil
	if res.Usage != (wire.Usage{}) {
		usage := res.Usage
		r.Usage = &usage
	}
	r.RanRoute = ranRoute(res)
	r.Attempts = nil
	if len(res.Hops) > 1 {
		r.Attempts = slices.Clone(res.Hops) // hop errors are redacted where hopOf builds them
	}
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
		r.Status, r.Error, r.FailKind = "error", redact.Text(textutil.RedactSecrets(err.Error())), failKind(err) // redact.Text: stderrTails can hold the env master key
	}
}

// ranRoute is the final hop's row as provider:profile, the value the ran-on line prints; empty when no row ran.
func ranRoute(res wire.Result) string {
	for _, h := range slices.Backward(res.Hops) {
		if h.NotStarted {
			continue
		}
		provider, profile, ok := strings.Cut(h.Account, "·")
		if !ok {
			return provider // an own-login account has no "·", e.g. opencode-go
		}
		return router.JoinCoworker(provider, cmp.Or(h.Profile, profile))
	}
	return ""
}

// RecordRefusal fails a never-launched ask refused at the route check: its job's record, else one new record.
func RecordRefusal(name string, req RunOptions, job *Record, err error) {
	hop := wire.Hop{Agent: name, Model: req.Model, ErrorKind: wire.ErrorKind(err), NotStarted: true}
	res := wire.Result{Agent: name, Invoked: req.Model, Hops: []wire.Hop{hop}}
	if job != nil {
		_ = job.Conclude(res, err, RunRecordedOpts{}) // a failed save must not mask the refusal
		return
	}
	rec := NewRecord(name, req, time.Now())
	rec.FallbackPolicy = req.Fallback
	rec.Finish(res, err, 0)
	rec.Attempts = res.Hops // Finish keeps attempts only past one hop
	claimRecord(rec)        // the one write is already concluded; a failed claim leaves nothing behind
}

// failKind: availability when no model could answer (limits, auth, routing, transport, timeout, cancel), else quality.
func failKind(err error) string {
	if _, ok := errors.AsType[*PreflightError](err); ok || errors.Is(err, ErrModelRoute) {
		return FailAvailability
	}
	switch wire.ErrorKind(err) {
	case "auth", "rate_limit", "quota", "transient", "timeout", "canceled":
		return FailAvailability
	}
	return FailQuality
}

// SetGates records the gate result (pass|fail|none) on a concluded dispatch; no caller sets it yet.
func SetGates(id, gates string) error {
	if gates != "pass" && gates != "fail" && gates != "none" {
		return fmt.Errorf("dispatch %s: gates %q is not pass, fail or none", id, gates)
	}
	_, _, err := updateRecord(id, func(r Record) (Record, bool) {
		r.Gates = gates
		return r, true
	})
	return err
}

func (r *Record) finishSave(res wire.Result, err error, took time.Duration, notify func(string)) {
	r.Error = ""
	r.Finish(res, err, took)
	_, _, serr := updateRecord(r.ID, func(current Record) (Record, bool) {
		// The runner has the actual outcome; only a failed run needs the provisional reason as context.
		if err != nil && current.Status == "error" && current.Error != "" && current.Error != r.Error {
			r.Error += "; " + current.Error
		}
		return *r, true
	})
	if serr != nil && notify != nil {
		notify(fmt.Sprintf("dispatch %s record not saved: %v", r.ID, serr))
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
		_ = os.Remove(filepath.Join(dir, name+".lock"))
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".json")+".log"))
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
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	return decodeRecord(b)
}

func decodeRecord(b []byte) (Record, error) {
	var r Record
	_, err := schemas.Decode(dispatchFormat, b, &r)
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
	return names, nil // ReadDir already sorts by filename
}

// LoadRecord reads one record by exact id or unique id prefix.
func LoadRecord(id string) (Record, error) {
	return loadRecord(id, recordNames)
}

// RecordIndex loads many records while listing the dispatch dir at most once; the zero value is ready.
type RecordIndex struct {
	names  []string
	listed bool
}

func (x *RecordIndex) list(dir string) ([]string, error) {
	if x.listed {
		return x.names, nil
	}
	names, err := recordNames(dir)
	if err == nil {
		x.names, x.listed = names, true
	}
	return names, err
}

// Load resolves each id like LoadRecord and skips the ones that do not resolve.
func (x *RecordIndex) Load(ids []string) []Record {
	var out []Record
	for _, id := range ids {
		if r, err := loadRecord(id, x.list); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// loadRecord resolves id with an exact read first, then a prefix match over the names list(dir) returns.
func loadRecord(id string, list func(dir string) ([]string, error)) (Record, error) {
	if id == "" { // an empty prefix would match every record
		return Record{}, errors.New("no matching dispatch found (see claude-code agents status)")
	}
	dir := paths.DispatchDir()
	name := id + ".json"
	if id == filepath.Base(id) && !strings.Contains(id, "..") {
		if r, err := readRecord(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			return r, err
		}
	}
	names, err := list(dir)
	if err != nil {
		return Record{}, fmt.Errorf("dispatch %q: %w", id, err)
	}
	var match []string
	for _, n := range names {
		if n == name {
			return readRecord(filepath.Join(dir, n))
		}
		if strings.HasPrefix(n, id) {
			match = append(match, n)
		}
	}
	if len(match) != 1 {
		return Record{}, fmt.Errorf("dispatch %q: %d matches (see claude-code agents status)", id, len(match))
	}
	return readRecord(filepath.Join(dir, match[0]))
}

// sameAgentRow reports a run on the asked agent, even after a move to another of its rows (Fallback is the asked name).
func (r Record) sameAgentRow() bool { return r.RanAs == "" || r.RanAs == r.Fallback }

// SessionModel is the newest dispatch's `<profile>:<served model>` (bare model with no profile); "" when unknown.
func SessionModel(session string) string {
	if session == "" {
		return ""
	}
	model := ""
	_ = eachRecent(paths.DispatchDir(), jsonToken(session), func(r Record) bool {
		if r.SessionID != session {
			return true
		}
		profile, invoked, pinned := strings.Cut(r.Invoked, ":")
		if !pinned || r.Model == r.Invoked { // served as typed: a local tag such as qwen3:30b, no profile prefix
			profile, invoked = "", r.Invoked
		}
		m := cmp.Or(r.Model, invoked)  // the served model, not the requested alias
		if strings.HasPrefix(m, "<") { // "<synthetic>" is no model
			m = invoked
		}
		if m == "" || strings.HasPrefix(m, "<") {
			return true
		}
		model = m
		if profile != "" && r.sameAgentRow() {
			model = profile + ":" + m
		}
		return false
	})
	return model
}

// AllRecords is RecentRecords' n for "every record": the log itself is capped by the dispatch keep.
const AllRecords = 1 << 20

// RecentRecords returns up to n records, newest first; an unreadable dispatch dir lists none.
func RecentRecords(n int) []Record {
	out, _ := ReadRecent(n)
	return out
}

// ReadRecent is RecentRecords that also returns an unreadable dispatch dir's error.
func ReadRecent(n int) ([]Record, error) { return ReadRecentWith(n, "") }

// ReadRecentWith is ReadRecent over the records whose file bytes hold needle (see eachRecent).
func ReadRecentWith(n int, needle string) ([]Record, error) {
	if n < 1 {
		return nil, nil
	}
	var out []Record
	err := eachRecent(paths.DispatchDir(), needle, func(r Record) bool {
		out = append(out, r)
		return len(out) < n
	})
	return out, err
}

// RunningRecent is ReadRecentWith over the records whose bytes mention a running status; callers still check Status.
func RunningRecent(n int) ([]Record, error) { return ReadRecentWith(n, inflight.RunningToken) }

// jsonToken is s as the record writer encodes it, the bytes any record holding s as a field value must contain.
func jsonToken(s string) string {
	if s == "" {
		return ""
	}
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// eachRecent calls fn newest first on each decodable record in dir holding needle (jsonToken, RunningToken, "" = all).
func eachRecent(dir, needle string, fn func(Record) bool) error {
	names, err := recordNames(dir)
	probe := []byte(needle)
	var buf []byte // one read buffer for every record
	for _, n := range slices.Backward(names) {
		var rerr error
		var b []byte
		b, buf, rerr = readInto(filepath.Join(dir, n), buf)
		if rerr != nil || !bytes.Contains(b, probe) {
			continue
		}
		if r, derr := decodeRecord(b); derr == nil && !fn(r) {
			break
		}
	}
	return err
}

// readInto reads path into buf's storage (grown as needed) and returns the bytes and the buffer to reuse.
func readInto(path string, buf []byte) ([]byte, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, buf, err
	}
	defer f.Close()
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, 4096)
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, buf, nil
		}
		if err != nil {
			return nil, buf, err
		}
	}
}

// RunRecordedOpts tunes runRecorded for the caller's stdout contract.
type RunRecordedOpts struct {
	Job      bool         // a --job child: stdout is the log, so out is dropped and the log keeps events + stderr only
	AnswerTo io.Writer    // written the final answer once the log is flushed (a job's `agents result` reads it)
	Notify   func(string) // receives non-fatal bookkeeping failures (the record not saving)
	Hold     bool         // a retried attempt: no answer, result event or Finish; the caller's Conclude does them once
}

// Conclude ends a Hold-attempt record and returns the error it recorded (a failed answer or event write).
func (r *Record) Conclude(res wire.Result, err error, o RunRecordedOpts) error {
	events := wire.NewEventLog(r.EventsPath(), r.ID)
	defer events.Close()
	return r.conclude(res, err, time.Since(r.Time), o, events.Emit)
}

// conclude is the one finish step: answer, result event, then the record save; it returns the error the record holds.
func (r *Record) conclude(res wire.Result, err error, took time.Duration, o RunRecordedOpts, emit func(wire.Event) error) error {
	if o.AnswerTo != nil && res.Answer != "" {
		if _, werr := io.WriteString(o.AnswerTo, res.Answer); werr != nil && err == nil {
			err = fmt.Errorf("write final answer: %w", werr)
		}
	}
	// the result event lands before the record leaves running, so a tail follower never exits without it
	if rerr := emit(wire.ResultEvent(wire.NewEnvelope(res, err, r.ID))); rerr != nil && err == nil {
		err = rerr
	}
	r.finishSave(res, err, took, o.Notify)
	return err
}

// runRecorded runs one recorded dispatch (log, event file, start, run, Finish/Save, result): one pair per id.
func runRecorded(ctx context.Context, rec *Record, agentName string, out wire.Sink, o RunRecordedOpts,
	run func(context.Context, wire.Sink) (wire.Result, error)) (wire.Result, error) {
	logSink := wire.NewLogSink(rec.LogPath())
	defer logSink.Close() // idempotent: covers a panic in Run
	ctx = wire.WithLog(ctx, logSink)
	ctx = withDispatchID(ctx, rec.ID, rec.Depth)
	if o.Job {
		ctx, out = wire.WithLogEvents(ctx, logSink), wire.Discard
	}
	events := wire.NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	sink := wire.Multi(out, events)
	begin := time.Now()
	res := wire.Result{Agent: agentName, Status: wire.StatusError}
	err := sink.Emit(wire.Event{Type: wire.EventStart, Agent: agentName, DispatchID: rec.ID, Status: "running"})
	if err != nil {
		logSink.Close()
		rec.finishSave(res, err, time.Since(begin), o.Notify)
		return res, err
	}
	res, err = run(ctx, sink)
	logSink.Close() // flush before writing the answer, so tail -f never misses the last line
	if o.Hold {
		return res, err
	}
	return res, rec.conclude(res, err, time.Since(begin), o, sink.Emit)
}

// lastResultEvent is the last result event's envelope in rec's events file; nil when none or the file is unreadable.
func (rec Record) lastResultEvent() (*wire.Envelope, error) {
	f, err := os.Open(rec.EventsPath())
	if err != nil {
		return nil, nil
	}
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
	return env, sc.Err()
}

// SavedEnvelope is the last result event's envelope, true only when one was found and the events file read cleanly.
func (rec Record) SavedEnvelope() (wire.Envelope, bool) {
	env, err := rec.lastResultEvent()
	if env == nil || err != nil {
		return wire.Envelope{}, false
	}
	return *env, true
}

// ResultEnvelope is the result event's envelope, or one rebuilt from rec when no result event can be read.
func (rec Record) ResultEnvelope(errw io.Writer) wire.Envelope {
	env, err := rec.lastResultEvent()
	if err != nil {
		fmt.Fprintf(errw, "agents result: %s: events unreadable, envelope may be incomplete: %v\n", rec.ID, err)
	}
	if env != nil {
		return *env
	}
	var runErr error
	if rec.Status == "error" {
		runErr = fmt.Errorf("%s", rec.Error)
	}
	res := wire.Result{Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model, Invoked: rec.Invoked,
		SessionID: rec.SessionID, DurationMs: rec.DurationMs, Fallback: rec.Fallback, FallbackReason: rec.FallbackReason}
	if rec.Usage != nil {
		res.Usage = *rec.Usage
	}
	if log, err := os.ReadFile(rec.LogPath()); err == nil {
		res.Answer = string(log)
	}
	return wire.NewEnvelope(res, runErr, rec.ID)
}

// buildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func buildAskRecord(ctx context.Context, name string, req RunOptions, jobRec *Record, workdir string, background bool) (Record, error) {
	ctx = valuesContext(ctx, req.Values)
	var rec Record
	if jobRec != nil {
		rec = *jobRec
	} else {
		rec = NewRecord(name, req, time.Now())
	}
	maxDepth := DefaultMaxDispatchDepth // a legacy job record carries no cap
	if rec.MaxDepth != nil {
		maxDepth = *rec.MaxDepth
	}
	if maxDepth > 0 && rec.Depth > maxDepth {
		return Record{}, &UsageError{Msg: fmt.Sprintf("dispatch depth %d exceeds cap %d (parent %s); "+
			"set %s (0 = no cap)", rec.Depth, maxDepth, rec.Parent, maxDispatchDepthEnv)}
	}
	if jobRec != nil {
		return rec, nil // same id: a job run continues the record it was spawned from
	}
	rec, err := claimLimitedRecord(ctx, name, rec)
	if err != nil {
		return Record{}, err
	}
	rec.Tree, rec.Repo = recordTree(ctx, workdir, req.WorkDir)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec, nil
}

// recordTree is the git tree and repo for workdir, joining Prepare's prefetch when it ran for the same directory.
func recordTree(ctx context.Context, workdir, reqWorkdir string) (tree, repo string) {
	if join, ok := ctx.Value(treeKey{}).(func() (string, string)); ok && workdir == reqWorkdir {
		return join()
	}
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return treeAndRepo(ctx, cwd)
}
