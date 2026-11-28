package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// layoutFile stamps the store root with layoutFlat, the one-bank layout.
const layoutFile = ".layout"

const storeVersion = 4 // the "flat N" this binary writes

// StoreVersion is the "flat N" this binary writes.
func StoreVersion() int { return storeVersion }

var layoutFlat = "flat " + strconv.Itoa(storeVersion)

// Path is one of the bank's top-level files.
func Path(name string) string { return filepath.Join(paths.MemoryStoreDir(), name) }

// nativeIndex is the one shared MEMORY.md.
func nativeIndex() string { return filepath.Join(paths.MemoryStoreDir(), "MEMORY.md") }

func ensure(sub string) error {
	if err := stampLayout(); err != nil {
		return err
	}
	return os.MkdirAll(paths.MemorySubDir(sub), 0o755)
}

// stampLayout refuses a newer or malformed stamp, rewrites an older one, and writes it when absent.
func stampLayout() error {
	p := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read memory store layout: %w", err)
	}
	if err == nil {
		v, err := StoreLayoutVersion(string(b))
		if err != nil {
			return err
		}
		if v > storeVersion {
			return fmt.Errorf("store is flat %d, this binary writes flat %d: install a newer claude-code", v, storeVersion)
		}
		if v == storeVersion {
			return nil
		}
	}
	return paths.AtomicWrite(p, []byte(layoutFlat+"\n"), 0o644)
}

// StoreLayoutVersion parses a positive, unsigned flat stamp for writers and doctor.
func StoreLayoutVersion(stamp string) (int, error) {
	stamp = strings.TrimSpace(stamp)
	n, ok := strings.CutPrefix(stamp, "flat ")
	n = strings.TrimSpace(n)
	v, err := strconv.Atoi(n)
	if !ok || err != nil || v <= 0 || strings.HasPrefix(n, "+") {
		return 0, fmt.Errorf("memory store layout %q is not a %q stamp", stamp, "flat N")
	}
	return v, nil
}

const homeDir = "home"

// flatSubs derives the store folders from the asset roles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// withBankLock is WithBankLock for writers.
func withBankLock(wait time.Duration, fn func() error) error {
	return WithBankLock(wait, func() error {
		if err := stampLayout(); err != nil {
			return err
		}
		return fn()
	})
}

// EnsureBank is the one bank with the store root created.
func EnsureBank(sub string) error {
	return WithBankLock(BankLockWait, func() error { return ensure(sub) })
}

// Version is an opaque per-entry token issued by the store; callers pass it back untouched and never parse it.
type Version string

// Versions maps an entry ref to its Version; "" marks a ref that is unknown or ambiguous, so not writable.
type Versions map[string]Version

// ConflictError is a typed Apply refusal: op Op named Ref at a Version other than the base it was read at.
type ConflictError struct {
	Op            int
	Ref           string
	Base, Current Version
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("op %d: %s changed since read (base %q, now %q)", e.Op, e.Ref, e.Base, e.Current)
}

// BankStore is the db seam typed writers use.
type BankStore interface {
	Load() (map[string]Doc, Versions, error)
	Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) // all or nothing
}

// OpenBankStore is nil when no db store is linked; the close func releases the db.
var OpenBankStore func(locked bool) (BankStore, func() error, error)

var errNoDBStore = errors.New("memory: no db store is linked")

// truthPath is the retired truth file of the files era; only retireFilesTruth reads it.
func truthPath() string { return filepath.Join(paths.StateDir(), "memory", "truth") }

// storedTruthNotDB is true only for a truth file read successfully that is not "db"; missing or unreadable is false.
func storedTruthNotDB() bool {
	b, err := os.ReadFile(truthPath())
	return err == nil && strings.TrimSpace(string(b)) != "db"
}

// retireFilesTruth rewrites a stored truth other than "db" to "db"; the caller has just run a conflict-free guard.
func retireFilesTruth() {
	if !storedTruthNotDB() {
		return
	}
	if err := paths.AtomicWrite(truthPath(), []byte("db\n"), 0o644); err != nil { // never fails the caller
		fmt.Fprintf(os.Stderr, "memory: retire the files truth: %v\n", err)
	}
}

// NewBankStore is the db store; locked means the caller holds the bank lock, else Apply takes it.
func NewBankStore(locked bool) BankStore { return newBankStore(locked) }

func newBankStore(locked bool) BankStore { return dbBankStore{locked: locked} }

// dbBankStore opens the db store for each call and closes it after, so callers need no close func.
type dbBankStore struct{ locked bool }

func (s dbBankStore) with(f func(BankStore) error) error {
	if OpenBankStore == nil {
		return errNoDBStore
	}
	st, closeFn, err := OpenBankStore(s.locked)
	if err != nil {
		return fmt.Errorf("open db store: %w", err)
	}
	err = f(st)
	if cerr := closeFn(); cerr != nil { // a committed batch must not look retryable
		fmt.Fprintf(os.Stderr, "memory: closing db store: %v\n", cerr)
	}
	return err
}

func (s dbBankStore) Load() (docs map[string]Doc, v Versions, err error) {
	err = s.with(func(st BankStore) (e error) { docs, v, e = st.Load(); return })
	return
}

// Apply commits b and exports the Markdown under one bank lock (taken here unless held); flock is not reentrant.
func (s dbBankStore) Apply(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	if s.locked {
		return s.applyLocked(b, base, now)
	}
	err = withBankLock(BankLockWait, func() (e error) { rs, e = s.applyLocked(b, base, now); return })
	return
}

func (s dbBankStore) applyLocked(b Batch, base Versions, now time.Time) (rs []OpResult, err error) {
	locked := dbBankStore{locked: true}
	if err = locked.with(func(st BankStore) (e error) { rs, e = st.Apply(b, base, now); return }); err != nil {
		return
	}
	clean, xerr := exportAfterApply()
	if xerr != nil {
		return rs, &afterCommitError{&committedErr{fmt.Errorf("batch committed in the db; markdown export: %w", xerr)}}
	}
	if clean {
		retireFilesTruth() // the guard imported the Markdown a files truth left behind, with no conflict kept apart
	}
	return
}

// exportAfterApply runs the guard after a db commit under the held bank lock; clean means it ran with no conflict.
func exportAfterApply() (clean bool, err error) {
	if OpenExportDB == nil {
		return false, nil
	}
	edb, closeDB, err := OpenExportDB()
	if err != nil {
		return false, err
	}
	indexErr, conflicts, err := guardPass(edb, paths.MemoryStoreDir())
	return len(conflicts) == 0 && err == nil && indexErr == nil, errors.Join(indexErr, err, closeDB())
}

// ApplyDocs runs b over a copy of docs; a failed op fails the batch, so a store writes nothing.
func ApplyDocs(docs map[string]Doc, b Batch, now time.Time) (map[string]Doc, []OpResult, error) {
	b.Ops = readAsLoaded(docs, slices.Clone(b.Ops))
	if err := b.validate(); err != nil {
		return nil, nil, err
	}
	out := maps.Clone(docs)
	results := runBatchOps(out, b, now)
	if n := countFailedOps(results); n > 0 {
		return out, results, fmt.Errorf("%d of %d ops failed: nothing written", n, len(b.Ops))
	}
	return out, results, nil
}

// readAsLoaded gives an edit with no Read the contentRef of its entry in docs, for a store that checked base itself.
func readAsLoaded(docs map[string]Doc, ops []Op) []Op {
	for i, o := range ops {
		name, _ := routeBatchOp(docs, o)
		r, ok := parseRef(o.ID)
		if !o.editsEntry() || o.Read != "" || name == "" || !ok || r.derived {
			continue
		}
		for _, e := range docs[name].Entries {
			if e.matches(r) {
				ops[i].Read = contentRef(e.block())
			}
		}
	}
	return ops
}

var planShaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// checkOpenPlanTasks verifies every plan task in tasks is still open before applying a dream batch.
func checkOpenPlanTasks(tasks []PlanTaskOp) error {
	for _, t := range tasks {
		if err := plans.CheckOpen(t.Plan, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// applyPlanTask closes a task only when plan and id are in the open-task list the model was shown.
func (r *dreamRan) applyPlanTask(t PlanTaskOp) error {
	if err := r.checkPlanTask(t); err != nil {
		return err
	}
	r.plan.checkpoint(t.Plan)
	var err error
	wrote := false
	switch t.Action {
	case "done":
		wrote, err = plans.MarkDone(t.Plan, t.ID, t.Sha)
	case "strike":
		wrote, err = plans.StrikeWith(t.Plan, t.ID, t.Reason, t.Evidence)
	}
	if err == nil && !wrote {
		return errors.New("a peer closed the task first")
	}
	if err == nil {
		r.plan.record(t.Plan, t.ID)
		r.note("plans/" + filepath.Base(t.Plan))
	}
	return err
}

func (r *dreamRan) checkPlanTask(t PlanTaskOp) error {
	if r.plan == nil {
		return errors.New("no plans in this checkout")
	}
	if !slices.ContainsFunc(r.plan.open, func(o plans.OpenTask) bool { return o.Plan == t.Plan && o.ID == t.ID }) {
		return errors.New("not an open task of the listed plans")
	}
	if !slices.ContainsFunc(plans.OpenTasks(r.plan.root), func(o plans.OpenTask) bool { return o.Plan == t.Plan && o.ID == t.ID }) {
		return errors.New("the task is already closed")
	}
	switch {
	case t.Action == "done" && planShaRE.MatchString(t.Sha):
		return nil
	case t.Action == "strike":
		return plans.CheckStrike(t.Reason, t.Evidence)
	default:
		return fmt.Errorf("action %q with sha %q is not valid", t.Action, t.Sha)
	}
}

// dreamPlans is the plan phase: the session checkout and its plans and citable evidence from before the run.
type dreamPlans struct {
	root   string
	before plans.Snapshot
	cite   plans.Cite
	open   []plans.OpenTask     // listed in the prompt; the only tasks a typed plan_tasks entry may close
	wrote  map[string]planWrite // typed plan_tasks writes by plan: what finish may undo
}

// planWrite is the tasks this run closed in one plan and the plan's bytes right after its last write.
type planWrite struct {
	ids     map[string]bool
	text    []byte
	tainted bool // a peer changed the plan between two of this run's writes: nothing earlier is ours to undo or drop
}

// owns is true only for a task this run wrote in a plan nobody has touched since (compare-and-swap on the bytes).
func (d *dreamPlans) owns(plan, id string) bool {
	w, ok := d.wrote[plan]
	cur, err := os.ReadFile(plan)
	return ok && w.ids[id] && err == nil && bytes.Equal(cur, w.text)
}

// checkpoint drops the earlier ids of plan when a peer changed it since this run's last write there.
func (d *dreamPlans) checkpoint(plan string) {
	if w, ok := d.wrote[plan]; ok {
		if cur, err := os.ReadFile(plan); err != nil || !bytes.Equal(cur, w.text) {
			w.ids, w.tainted = nil, true
			d.wrote[plan] = w
		}
	}
}

// record notes that this run just wrote id in plan.
func (d *dreamPlans) record(plan, id string) {
	if d.wrote == nil {
		d.wrote = map[string]planWrite{}
	}
	w := d.wrote[plan]
	if w.ids == nil {
		w.ids = map[string]bool{}
	}
	w.ids[id] = true
	w.text, _ = os.ReadFile(plan) // a failed read leaves nil, so owns is false
	d.wrote[plan] = w
}

// startPlanPhase snapshots the checkout's plans; nil when cwd is no checkout or has no plans home.
func startPlanPhase(cwd string) *dreamPlans {
	top := GitToplevel(cwd)
	if top == "" {
		return nil
	}
	if _, err := os.Stat(plans.Home(top)); err != nil {
		return nil
	}
	return &dreamPlans{root: top, before: plans.TakeSnapshot(top), cite: storeCites(), open: plans.OpenTasks(top)}
}

// promptBlock is the open tasks the plan phase judges, as compact JSON; empty when no recent plan has one.
func (d *dreamPlans) promptBlock() string {
	if d == nil {
		return ""
	}
	b, err := json.Marshal(d.open)
	if len(d.open) == 0 || err != nil {
		return ""
	}
	return assets.MemoryOpenTasks(string(b))
}

// finish re-checks every plan change made during the run and prints one line; it never fails the dream.
func (d *dreamPlans) finish(out io.Writer) []plans.Change {
	if d == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	mine := map[[2]string]bool{} // judged before any revert rewrites a plan's bytes
	for plan, w := range d.wrote {
		for id := range w.ids {
			mine[[2]string{plan, id}] = d.owns(plan, id)
		}
	}
	own := func(plan, id string) bool { return mine[[2]string{plan, id}] }
	rep, err := plans.RecheckOwned(ctx, d.root, jobDream, d.before, d.cite, own)
	if err == nil && len(rep.Kept)+len(rep.Reverted)+len(rep.Flagged) == 0 && !rep.TextChanged {
		return nil
	}
	line := rep.Line()
	if err != nil {
		line += "; re-check errors: " + strings.Join(strings.Fields(err.Error()), " ")
	}
	fmt.Fprintln(out, "memory dream:", line)
	return rep.Reverted
}

// dropReverted removes the changed entry of each plan whose every write of this run finish reverted.
func (r *dreamRan) dropReverted(reverted []plans.Change) {
	if r.plan == nil {
		return
	}
	for plan, w := range r.plan.wrote {
		left := len(w.ids)
		if w.tainted {
			continue
		}
		for id := range w.ids {
			if slices.ContainsFunc(reverted, func(c plans.Change) bool { return c.Plan == plan && c.ID == id }) {
				left--
			}
		}
		if left == 0 {
			r.changed = slices.DeleteFunc(r.changed, func(n string) bool { return n == "plans/"+filepath.Base(plan) })
		}
	}
}

// storeCites is the evidence a strike may cite, read before the run so the job cannot cite what it wrote.
func storeCites() plans.Cite {
	ids := map[string]bool{}
	logs := append([]string{Path("decisionLog.md")}, transcript.GlobIn(filepath.Join(paths.MemoryStoreDir(), "archive"), "decisionLog*.md")...)
	for _, f := range logs {
		b, _ := os.ReadFile(f) // a missing log cites nothing
		for _, m := range idMarker().FindAllStringSubmatch(string(b), -1) {
			ids[m[1]] = true
		}
	}
	notes := map[string]bool{}
	for _, dir := range []string{"feedback", "notes"} {
		for _, f := range transcript.GlobIn(filepath.Join(paths.MemoryStoreDir(), dir), "*.md") {
			notes[dir+"/"+filepath.Base(f)] = true
		}
	}
	return func(kind, ref string) bool { return kind == "decision" && ids[ref] || kind == "note" && notes[ref] }
}
