package plans

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// AutofillEnv is the vault env key whose off|false|0 stops automatic fill without a build.
const AutofillEnv = "HARNESS_AUTOFILL"

// AutofillOrigin prefixes every automatic dispatch's batch id, so its record's task_id names where it came from.
const AutofillOrigin = "autofill"

// AutofillInput is a ready read plus the session whose spend cap applies ("" checks the day cap only).
type AutofillInput struct {
	ReadyInput
	Session string
}

// AutofillResult names each task sent ("<plan-slug> <id>") and each held back with why; Lines are its notices.
type AutofillResult struct {
	Sent, Held, Lines []string
}

// autofillTiers is each leaf agent's sonnet-tier model first, then the lower ones a line may name (R35, 11.5).
var autofillTiers = map[string][]string{
	"claude": {"sonnet", "haiku"}, "pi": {"sonnet", "haiku"}, "copilot": {"sonnet", "haiku"},
	"codex": {"terra", "luna"}, "gemini": {"gemini-4-neon", "gemini-4-hydrogen"}, "opencode": {"kimi-k3", "kimi-k3-1-pro"},
}

// overTier are the words that ask for more than the sonnet tier, on a leaf: value or a brief's Tier: line.
var overTier = []string{"opus", "fable", "sol", "astra", "argon", "pro", "ultra", "deep"}

var (
	briefTier    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?mi)^[\s>*-]*Tier:[ \t]*(.+)`) })
	modelWord    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[a-z0-9][a-z0-9.-]*`) })
	autofillTask = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9._ -]{1,64}$`) })
	idUnsafe     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[^A-Za-z0-9._-]+`) })
)

// Seams: the detached spawn and the binary it runs.
var (
	autofillSpawn = func(bin string, args []string, dir string, out *os.File) int {
		return proc.SpawnDetachedPID(bin, args, dir, out)
	}
	autofillBin = paths.SelfBinary
)

// autofillOff is the off key; any other value, unset included, leaves automatic fill on (owner, 2026-11-26).
func autofillOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AutofillEnv))) {
	case "off", "false", "0":
		return true
	}
	return false
}

// autofillFresh is how long a spawned batch counts against the slots before its agent shows in the live count.
const autofillFresh = 30 * time.Second

// autofillLockWait keeps a waiting trigger well inside the 10 s docs-plan hook budget.
const autofillLockWait = 2 * time.Second

func autofillDir() string { return filepath.Join(paths.StateDir(), AutofillOrigin) }

// autofillDeadline bounds a whole run inside the hook; past it the run is left behind (spawns are detached anyway).
var autofillDeadline = 500 * time.Millisecond

// Autofill dispatches each ready task with a brief while a slot is free, claiming it first; no model decides.
func Autofill(in AutofillInput) AutofillResult {
	if autofillOff() {
		return AutofillResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), autofillDeadline)
	defer cancel() // a worker past the deadline sees ctx done and claims and spawns nothing
	done := make(chan AutofillResult, 1)
	go func() {
		var res AutofillResult
		// one run at a time, so two triggers that read the same free slots cannot both fill them
		err := paths.WithFileLockWithin(filepath.Join(autofillDir(), "run"), autofillLockWait, func() error {
			res = autofillLocked(ctx, in)
			return nil
		})
		if err != nil {
			res.Lines = append(res.Lines, "plans: autofill stopped: "+err.Error())
		}
		done <- res
	}()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		return AutofillResult{Lines: []string{"plans: autofill stopped: past its " + autofillDeadline.String() + " deadline"}}
	}
}

// recentSpawns counts batches started within autofillFresh, which the caller's slot count may not show yet.
func recentSpawns() int {
	entries, _ := os.ReadDir(autofillDir()) // none yet is none recent
	n := 0
	for _, e := range entries {
		if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".jsonl") && time.Since(info.ModTime()) < autofillFresh {
			n++
		}
	}
	return n
}

// autofillKeep is how long a batch file and its log stay for the stats week (11.4) and the attempt count.
const autofillKeep = 7 * 24 * time.Hour

// batchLine is the one line of batch id's file; empty when the file is gone or unreadable.
func batchLine(id string) (line struct{ Workdir, Prompt string }) {
	if b, err := os.ReadFile(filepath.Join(autofillDir(), id+".jsonl")); err == nil {
		_ = json.Unmarshal(b, &line) // an unreadable line stays empty
	}
	return line
}

// batchRoot is the workdir a batch id was started for; "" when its batch file is gone or unreadable.
func batchRoot(id string) string { return batchLine(id).Workdir }

// pruneAutofill removes root's batch files and logs older than autofillKeep, and orphans; a failed removal waits.
func pruneAutofill(root string) {
	entries, _ := os.ReadDir(autofillDir()) // none yet is nothing to prune
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), "run.") || time.Since(info.ModTime()) <= autofillKeep {
			continue
		}
		id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if wd := batchRoot(id); wd == "" || wd == root {
			_ = os.Remove(filepath.Join(autofillDir(), e.Name()))
		}
	}
}

// autofillStale is the age past which a batch with no envelope and no live process crashed (asks time out at 15m).
const autofillStale = 30 * time.Minute

// crashed is a batch that wrote no envelope, is older than autofillStale and whose spawned process is gone.
func crashed(id string) (string, bool) {
	info, err := os.Stat(filepath.Join(autofillDir(), id+".jsonl"))
	if err != nil || time.Since(info.ModTime()) < autofillStale {
		return "", false // a pruned or young batch is not judged
	}
	b, err := os.ReadFile(filepath.Join(autofillDir(), id+".pid"))
	if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 && proc.Alive(pid) {
		return "", false
	}
	if status, ok := recordedStatus(id, info.ModTime().Add(-time.Hour)); ok { // an hour of slack for a clock step
		return status, status != "running" // the record, reaped on read, counted this run's failure
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "unsent", true // a spawned run always leaves a pid file or a record, so this one never ran
	}
	return "crashed", true
}

// BatchRecord is the reaped status of the dispatch record for batch id written since since; nil links no records.
var BatchRecord func(id string, since time.Time) (status string, ok bool)

// FailedAttempts is how many of a plan task's latest dispatches failed in a row; nil links no records.
var FailedAttempts func(planTask string) int

// recordedStatus is BatchRecord's answer, or none when no record store is linked.
func recordedStatus(id string, since time.Time) (string, bool) {
	if BatchRecord == nil {
		return "", false
	}
	return BatchRecord(id, since)
}

// batchOutcome is batch id's final status ("ok", "error", "crashed", ...) once its run concluded; false while not.
func batchOutcome(id string) (string, bool) {
	b, _ := os.ReadFile(filepath.Join(autofillDir(), id+".log")) // no log is no envelope
	for line := range strings.SplitSeq(string(b), "\n") {
		var env struct {
			TaskID   string `json:"task_id"`
			Dispatch string `json:"dispatch_id"`
			Status   string `json:"status"`
		}
		if json.Unmarshal([]byte(line), &env) != nil || env.TaskID != id || env.Status == "" {
			continue
		}
		var rec struct {
			Status string `json:"status"`
		}
		if rb, err := os.ReadFile(filepath.Join(paths.DispatchDir(), env.Dispatch+".json")); env.Dispatch != "" && err == nil &&
			json.Unmarshal(rb, &rec) == nil && rec.Status != "" {
			env.Status = rec.Status // the record is the truth; the envelope stands in when it is gone
		}
		return env.Status, env.Status != "running"
	}
	return crashed(id)
}

var autofillClaim = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^working: ` + AutofillOrigin + ` (` + AutofillOrigin + `-[A-Za-z0-9._-]+) `)
})

// releaseConcluded frees each autofill claim whose run concluded not ok, so the agent's attempt count governs a retry.
func releaseConcluded(in ReadyInput) []string {
	files := in.Plans
	if files == nil {
		files = activePlans(Home(in.Root))
	}
	var lines []string
	for _, f := range files {
		p, err := Parse(f)
		if err != nil {
			continue
		}
		for _, t := range p.Tasks {
			if l := releaseOne(f, t); l != "" {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

// releaseOne frees t's autofill claim in plan f when its run concluded not ok; the notice line, else "".
func releaseOne(f string, t Task) string {
	m := autofillClaim().FindStringSubmatch(t.Claim)
	if m == nil || !open(t) || t.ID == "" {
		return ""
	}
	status, done := batchOutcome(m[1])
	if !done || status == "ok" { // an ok run's claim stays until its tick closes the task
		return ""
	}
	key := strings.TrimSuffix(filepath.Base(f), ".md") + " " + taskKey(t)
	if err := ClearClaimOwned(f, t.ID, m[1]); err != nil {
		return "plans: autofill could not release " + key + ": " + err.Error()
	}
	return "plans: autofill released " + key + ": its run ended " + status
}

func autofillLocked(ctx context.Context, in AutofillInput) AutofillResult {
	res := AutofillResult{Lines: releaseConcluded(in.ReadyInput)} // before the prune, which drops week-old logs
	pruneAutofill(in.Root)
	if in.Slots <= 0 {
		return res
	}
	rep, err := Ready(in.ReadyInput)
	if err != nil {
		res.Lines = append(res.Lines, "plans: autofill stopped: "+err.Error())
		return res
	}
	slots := in.Slots - recentSpawns()
	if slots <= 0 {
		return res
	}
	var chosen []string // owns of the tasks sent this run: a snapshot lists overlapping tasks as ready together
	for _, r := range rep.Ready {
		if slots <= 0 || ctx.Err() != nil {
			break
		}
		j, why, stop := autofillTry(ctx, in, r, chosen)
		if stop {
			res.Lines = append(res.Lines, "plans: autofill stopped: "+why)
			return res
		}
		if why != "" {
			res.Held = append(res.Held, j.key+": "+why)
			if r.Brief == NeedsBrief {
				why += ": " + r.Command
			}
			res.Lines = append(res.Lines, "plans: autofill held "+j.key+": "+why)
			continue
		}
		chosen = append(chosen, r.Owns...)
		slots--
		res.Sent = append(res.Sent, j.key)
		res.Lines = append(res.Lines, "plans: autofill sent "+j.key+" to "+j.agent+" at "+j.model)
	}
	return res
}

// autofillTry builds and launches r's job unless its owns overlap taken; why is "" once it is sent.
func autofillTry(ctx context.Context, in AutofillInput, r ReadyTask, taken []string) (autofillJob, string, bool) {
	j, why, stop := autofillJobFor(in, r)
	switch {
	case why != "" || stop:
	case ownsOverlapAny(taken, r.Owns):
		why = "its owns overlap a task sent in this run"
	default:
		why = j.launch(ctx)
	}
	return j, why, stop
}

// ownsOverlapAny reports whether any of owns overlaps a path in taken ("none" owns nothing).
func ownsOverlapAny(taken, owns []string) bool {
	for _, o := range owns {
		if strings.EqualFold(o, "none") {
			continue
		}
		if slices.ContainsFunc(taken, func(t string) bool { return OwnsOverlap(o, t) }) {
			return true
		}
	}
	return false
}

// autofillJob is one automatic dispatch: the task, its "<plan-slug> <id>" key, the agent, model and brief sent.
type autofillJob struct {
	root, planID              string // planID is the plan's own id for the task, the one ClaimFree finds
	task                      ReadyTask
	key, agent, model, prompt string
}

// autofillJobFor builds r's job, or why it is held; stop means nothing more may go out (spend cap).
func autofillJobFor(in AutofillInput, r ReadyTask) (autofillJob, string, bool) {
	j := autofillJob{root: in.Root, task: r, key: strings.TrimSuffix(filepath.Base(r.Plan), ".md") + " " + r.ID}
	t, leaf, ok := planLine(r)
	if !ok {
		return j, "its task is gone from the plan", false
	}
	var why string
	j.planID = t.ID
	if j.agent, j.model, j.prompt, why = autofillPick(r, leaf); why != "" {
		return j, why, false
	}
	if FailedAttempts != nil { // a task that keeps failing waits for a person; re-sending it each tick only spends
		if n := FailedAttempts(j.key); n >= 2 {
			return j, strconv.Itoa(n) + " failed attempts in a row, automatic fill stopped", false
		}
	}
	return j, "", false
}

// autofillPick is r's agent, its sonnet-tier model and the brief text, or why r is only announced.
func autofillPick(r ReadyTask, leaf string) (agent, model, prompt, why string) {
	if r.Brief == NeedsBrief {
		return "", "", "", NeedsBrief
	}
	if r.Leaf == "" || r.Verify == "" || len(r.Owns) == 0 {
		return "", "", "", "its line lacks leaf, owns or verify"
	}
	agent = strings.ToLower(r.Leaf)
	tiers, ok := autofillTiers[agent]
	if !ok {
		return "", "", "", "no sonnet-tier model known for " + agent
	}
	b, err := os.ReadFile(r.Brief)
	if err != nil {
		return "", "", "", "brief unreadable: " + err.Error()
	}
	prompt = string(b)
	asks := leaf
	if m := briefTier().FindStringSubmatch(prompt); m != nil {
		asks += " " + m[1]
	}
	model, why = tierModel(tiers, asks)
	return agent, model, prompt, why
}

// tierModel is the family's sonnet-tier model, or a lower one asks names; why when asks wants more (R35).
func tierModel(tiers []string, asks string) (string, string) {
	model := tiers[0]
	for _, w := range modelWord().FindAllString(strings.ToLower(asks), -1) {
		m := tierMatch(tiers, w) // a capped name first, so kimi-k3-1-pro is not read as "pro"
		if m != "" && model == tiers[0] {
			model = m
		} else if m == "" && containsWord(overTier, w) {
			return "", "asks for " + w + ", above the sonnet tier automatic fill sends (R35)"
		}
	}
	return model, ""
}

// tierMatch is the model of tiers w names, whole or as its last part ("neon", "k3"); "" for none.
func tierMatch(tiers []string, w string) string {
	for _, t := range tiers {
		if w == t || strings.HasSuffix(t, "-"+w) || strings.TrimPrefix(t, "kimi-") == w {
			return t
		}
	}
	return ""
}

func containsWord(words []string, w string) bool {
	for _, o := range words {
		if w == o || strings.HasPrefix(w, o+"-") || strings.Contains(w, "-"+o) {
			return true
		}
	}
	return false
}

// planLine is r's plan task (r.ID is its taskKey, "Phase 1.1" for "1.1") and its whole leaf: or agent: value.
func planLine(r ReadyTask) (Task, string, bool) {
	b, err := os.ReadFile(r.Plan)
	if err != nil {
		return Task{}, "", false
	}
	for _, t := range parseBytes(r.Plan, b).Tasks {
		if t.ID != "" && open(t) && taskKey(t) == r.ID {
			attrs := waveAttributes(ownLine(strings.Split(string(b), "\n"), t))
			return t, cmp.Or(attrs["agent"], attrs["leaf"]), true
		}
	}
	return Task{}, "", false
}

// launch claims the task, then starts one detached `agents ask --batch` set only by --overlay; why on failure.
func (j autofillJob) launch(ctx context.Context) string {
	if ctx.Err() != nil {
		return "past the autofill deadline"
	}
	if !autofillTask().MatchString(j.key) {
		return "plan task " + j.key + " does not fit a dispatch record's plan_task"
	}
	var rnd [3]byte
	_, _ = rand.Read(rnd[:]) // zeros still leave the task id in the batch id
	id := AutofillOrigin + "-" + hex.EncodeToString(rnd[:]) + "-" + strings.Trim(idUnsafe().ReplaceAllString(j.task.ID, "-"), "-")
	id = id[:min(len(id), 64)]
	claim := "working: " + AutofillOrigin + " " + id + " · worktree main · since " + time.Now().Format("15:04")
	batch, err := j.write(id) // before the claim: a crash between them leaves a batch the crashed sweep releases
	if err != nil {
		return "dispatch: " + err.Error()
	}
	if err := ClaimFree(j.task.Plan, j.planID, claim); err != nil {
		if ClearClaimOwned(j.task.Plan, j.planID, id) == nil { // a claim that did land keeps its batch for the sweep
			err = errors.Join(err, os.Remove(batch))
		}
		return "claim: " + err.Error()
	}
	if ctx.Err() != nil { // the deadline passed during the claim: release it and spawn nothing
		return "past the autofill deadline" + releaseUnsent(j, id, batch)
	}
	if err := j.start(ctx, id, batch); err != nil {
		if errors.Is(err, errPastDeadline) {
			return "past the autofill deadline" + releaseUnsent(j, id, batch)
		}
		if cerr := ClearClaimOwned(j.task.Plan, j.planID, id); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return "dispatch: " + err.Error()
	}
	return ""
}

// releaseUnsent frees j's claim and batch of a dispatch that never started; "" or the failure to release.
func releaseUnsent(j autofillJob, id, batch string) string {
	if err := ClearClaimOwned(j.task.Plan, j.planID, id); err != nil {
		return ": " + err.Error() // the batch stays with no pid file, so the sweep releases it as unsent, uncounted
	}
	if err := os.Remove(batch); err != nil {
		return ": " + err.Error()
	}
	return ""
}

// write saves the one batch line and returns its path.
func (j autofillJob) write(id string) (string, error) {
	tools := true
	line, err := json.Marshal(struct { // the line's overlay is its only setting; a line overlay model is typed (overlay-p1b)
		ID       string            `json:"id"`
		Agent    string            `json:"agent"`
		Overlay  map[string]string `json:"overlay"`
		Prompt   string            `json:"prompt"`
		Workdir  string            `json:"workdir"`
		Tools    *bool             `json:"tools"`
		PlanTask string            `json:"plan_task"`
	}{id, j.agent, map[string]string{"model": j.model}, j.prompt, j.root, &tools, j.key})
	if err != nil {
		return "", err
	}
	dir := autofillDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	batch := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(batch, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // a reused id never takes another batch's files
	if err != nil {
		return "", err
	}
	_, err = f.Write(append(line, '\n'))
	return batch, errors.Join(err, f.Close())
}

var errPastDeadline = errors.New("past the autofill deadline")

// start spawns the batch run, output kept beside it for the stats week (11.4); none once ctx is done.
func (j autofillJob) start(ctx context.Context, id, batch string) error {
	bin, err := autofillBin()
	if err != nil {
		return errors.Join(err, os.Remove(batch))
	}
	if ctx.Err() != nil { // the lookup outlasted the deadline: the caller releases the claim
		return errPastDeadline
	}
	dir := autofillDir()
	out, err := os.OpenFile(filepath.Join(dir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errors.Join(err, os.Remove(batch)) // an unstarted batch must not hold a slot (recentSpawns)
	}
	defer out.Close()
	args := []string{"agents", "ask", "--batch", batch, "--format", "json"}
	if ctx.Err() != nil { // the log open outlasted the deadline
		_ = os.Remove(filepath.Join(dir, id+".log"))
		return errPastDeadline
	}
	pid := autofillSpawn(bin, args, j.root, out)
	if pid <= 0 {
		return errors.Join(errors.New("the batch run did not start"), os.Remove(batch))
	}
	// never an error now: the run is live, so its claim must stay; a missing pid only matters past autofillStale
	_ = os.WriteFile(filepath.Join(dir, id+".pid"), []byte(strconv.Itoa(pid)), 0o600)
	return nil
}
