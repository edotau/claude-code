package memory

// The journaled commit transaction: journal under state BEFORE any write, capture the native
// revisions the batch replaces, apply with preimage checks, commit, then retire the journal.
// A crash at any step is finished by the next call.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// Change is one store-relative write or deletion; the bytes present at journal time become its preimage,
// so a replay never clobbers a later edit.
type Change struct {
	Rel    string // store-relative, validated like Commit
	Data   []byte // new bytes; ignored when Delete
	Delete bool
}

// Receipt records one applied batch; it is returned to the caller and never written.
type Receipt struct {
	ID          string
	Commit      string
	Applied     []string // written or removed by this batch
	Skipped     []string // already at Data (or deleted and absent)
	Divergent   []string // preimage mismatch; left untouched
	Preexisting []string // dirty native paths outside the batch, swept into Commit
}

// errDivergent accompanies a receipt when some checked changes found other bytes; what applied is committed.
var errDivergent = errors.New("memory transaction: live bytes diverged from the journaled preimage")

// journal is the durable pre-write record of a batch; state is "pending" → "applied" → removed when complete.
type journal struct {
	Schema  int             `json:"schema"`
	ID      string          `json:"id"`
	Subject string          `json:"subject"`
	State   string          `json:"state"`
	Changes []journalChange `json:"changes"`
}

type journalChange struct {
	Rel            string `json:"rel"`
	Delete         bool   `json:"delete"`
	HasPreimage    bool   `json:"hasPreimage"`
	PreimageSHA256 string `json:"preimageSha256,omitempty"`
	Data           []byte `json:"data,omitempty"` // base64 in JSON
}

// outcome is what check decided for one change against the live bytes.
type outcome int

const (
	outApply   outcome = iota // preimage matches (or absent as expected): write/remove
	outSkip                   // already at Data, or deleted and absent: an earlier attempt applied it
	outDiverge                // other bytes: leave untouched, report
)

// commitChangesLocked applies changes while callers hold the writable bank lock.
func commitChangesLocked(subject string, changes []Change) (Receipt, error) {
	skipped, err := recoverEach() // a journal that cannot be finished must not wedge new batches
	if err != nil {
		return Receipt{}, err
	}
	repo := StoreRepo()
	for _, c := range changes {
		if err := repo.validateRel(c.Rel); err != nil {
			return Receipt{}, err
		}
	}
	j, err := newJournal(repo, subject, changes)
	if err != nil {
		return Receipt{}, err
	}
	if err := j.save(); err != nil {
		return Receipt{}, err
	}
	rec, err := j.run(repo)
	if err != nil && skipped != nil { // a stuck journal stays on disk and is retried by the next commit
		err = errors.Join(err, skipped)
	}
	return rec, err
}

// recoverEach finishes every unfinished journal, oldest first, each as its own commit; a failing one is left on
// disk (skipped) so later ones still run, and a skipped one may replay after newer commits, where its preimage
// hashes make it diverge instead of clobber. err is a failed directory scan; skipped joins the per-journal failures.
func recoverEach() (skipped, err error) {
	entries, err := os.ReadDir(JournalDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries { // ReadDir sorts by name: the id's timestamp prefix makes that oldest first
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := recoverJournal(filepath.Join(JournalDir(), e.Name())); err != nil {
			skipped = errors.Join(skipped, err)
		}
	}
	return skipped, nil
}

// recoverJournal replays the journal in file f.
func recoverJournal(f string) error {
	data, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	var j journal
	if _, err := schemas.Decode(journalFormat, data, &j); err != nil { // ErrNewer: untouched, never replayed
		return fmt.Errorf("memory journal %s: %w", f, err)
	}
	if j.ID == "" || j.path() != f { // the body's id names where save/retire write: it must be this file
		return fmt.Errorf("memory journal %s: id %q does not match its file name", f, j.ID)
	}
	if _, err := j.run(StoreRepo()); err != nil && !errors.Is(err, errDivergent) {
		return err
	}
	return nil
}

// newJournal records the batch: preimages as hashes, new bytes verbatim, so replay needs nothing from the caller.
// Each change hashes the live bytes (none when absent): replay then tells "not applied" from "edited since".
func newJournal(repo Repo, subject string, changes []Change) (*journal, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	j := &journal{ID: time.Now().UTC().Format("20060102-150405.000") + "-" + hex.EncodeToString(rnd[:]), Subject: subject, State: "pending", Schema: journalSchema}
	for _, c := range changes {
		pre, err := os.ReadFile(filepath.Join(repo.Dir, c.Rel)) // nil when absent
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		jc := journalChange{Rel: c.Rel, Delete: c.Delete, HasPreimage: pre != nil}
		if pre != nil {
			jc.PreimageSHA256 = textutil.SHA256Hex(pre)
		}
		if !c.Delete {
			jc.Data = c.Data
		}
		j.Changes = append(j.Changes, jc)
	}
	return j, nil
}

func (j *journal) path() string { return filepath.Join(JournalDir(), j.ID+".json") }

// save writes the journal atomically under state; it is never committed.
func (j *journal) save() error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(JournalDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(j.path(), data, 0o600)
}

// run drives one journal: check, native capture, apply, commit, retire; a failed commit stays "applied".
func (j *journal) run(repo Repo) (Receipt, error) {
	rec := Receipt{ID: j.ID}
	present, err := j.applyBatch(repo, &rec)
	if err != nil {
		return rec, err
	}
	j.State = "applied"
	if err := j.save(); err != nil {
		return rec, err
	}
	if stage := commitStage(rec, present); len(stage) > 0 {
		if err := j.commit(repo, &rec, stage); err != nil {
			return rec, err
		}
	}
	if err := os.Remove(j.path()); err != nil && !os.IsNotExist(err) {
		return rec, err
	}
	if len(rec.Divergent) > 0 {
		return rec, errDivergent
	}
	return rec, nil
}

// applyBatch validates, plans, captures native revisions and applies every change; present lists the dirty files.
func (j *journal) applyBatch(repo Repo, rec *Receipt) (present []string, err error) {
	for _, c := range j.Changes { // replayed journals need path validation before any read
		if err := repo.validateRel(c.Rel); err != nil {
			return nil, err
		}
	}
	if err := repo.Ensure(); err != nil {
		return nil, err
	}
	present, err = dirtyFiles(repo)
	if err != nil {
		return nil, err
	}
	present = repo.dropCredentialShaped(present, j.touches) // an unrelated dir's credential file must not wedge this batch
	outcomes, capture, err := j.plan(repo.Dir, present, rec)
	if err != nil {
		return nil, err
	}
	if err := j.captureNative(repo, capture); err != nil {
		return nil, err
	}
	for i, c := range j.Changes {
		if err := c.apply(repo, outcomes[i], rec); err != nil {
			return nil, err
		}
	}
	return present, nil
}

// touches reports whether rel sits under a top-level dir (or is a root file) this journal changes.
func (j *journal) touches(rel string) bool {
	top, _, _ := strings.Cut(rel, "/")
	return slices.ContainsFunc(j.Changes, func(c journalChange) bool {
		t, _, _ := strings.Cut(c.Rel, "/")
		return t == top
	})
}

// commitStage lists what the batch commit stages: applied and preexisting paths, plus skipped ones left dirty by
// an earlier attempt of this journal (committed with no duplicate record).
func commitStage(rec Receipt, present []string) []string {
	stage := append(append([]string{}, rec.Applied...), rec.Preexisting...)
	for _, p := range rec.Skipped {
		if slices.Contains(present, p) {
			stage = append(stage, p)
		}
	}
	return stage
}

// plan checks every change against the live bytes and sorts the dirty files: outside the batch they are Preexisting;
// inside it and about to be applied they hold a native revision the write would erase, so they are captured first.
func (j *journal) plan(dir string, present []string, rec *Receipt) (outcomes []outcome, capture []string, err error) {
	outcomes = make([]outcome, len(j.Changes))
	batch := map[string]outcome{}
	for i, c := range j.Changes {
		if outcomes[i], err = c.check(dir); err != nil {
			return nil, nil, err
		}
		batch[c.Rel] = outcomes[i]
	}
	for _, p := range present {
		if o, inBatch := batch[p]; !inBatch {
			rec.Preexisting = append(rec.Preexisting, p)
		} else if o == outApply {
			capture = append(capture, p)
		}
	}
	return outcomes, capture, nil
}

// captureNative commits, ahead of the batch, the dirty revisions it replaces so the write cannot erase them.
func (j *journal) captureNative(repo Repo, capture []string) error {
	if len(capture) == 0 {
		return nil
	}
	_, _, err := repo.Commit(capture, "native: before "+j.Subject, fmt.Sprintf("captured %d", len(capture)))
	return err
}

// commit stages the batch and commits it; Commit stays empty when nothing changed.
func (j *journal) commit(repo Repo, rec *Receipt, stage []string) error {
	body := fmt.Sprintf("applied %d, skipped %d, divergent %d, preexisting %d",
		len(rec.Applied), len(rec.Skipped), len(rec.Divergent), len(rec.Preexisting))
	sha, committed, err := repo.Commit(stage, j.Subject, body)
	if committed {
		rec.Commit = sha
	}
	return err
}

// check reads the live bytes once and decides the change's outcome; every change is conditional on its preimage.
func (c journalChange) check(dir string) (outcome, error) {
	cur, err := os.ReadFile(filepath.Join(dir, c.Rel))
	absent := err != nil && os.IsNotExist(err)
	if err != nil && !absent {
		return 0, err
	}
	switch {
	case c.alreadyApplied(cur, absent):
		return outSkip, nil
	case c.preimageHolds(cur, absent):
		return outApply, nil
	}
	return outDiverge, nil
}

// alreadyApplied reports whether the live bytes already show the change: a delete of an absent file or the same data.
func (c journalChange) alreadyApplied(cur []byte, absent bool) bool {
	return c.Delete && absent || !c.Delete && !absent && bytes.Equal(cur, c.Data)
}

// preimageHolds reports whether the live bytes are still the change's recorded preimage (or absent when it had none).
func (c journalChange) preimageHolds(cur []byte, absent bool) bool {
	return !c.HasPreimage && absent || c.HasPreimage && !absent && textutil.SHA256Hex(cur) == c.PreimageSHA256
}

// apply rechecks the live bytes right before mutating (native capture ran git in between) and sorts the rel into
// Applied, Skipped or Divergent; bytes that became applicable only after plan were never captured, so they diverge.
func (c journalChange) apply(repo Repo, planned outcome, rec *Receipt) error {
	o, err := c.check(repo.Dir)
	if err != nil {
		return err
	}
	if o == outApply && planned != outApply {
		o = outDiverge
	}
	if c.recordUnapplied(o, rec) {
		return nil
	}
	if err := repo.validateRel(c.Rel); err != nil { // again, right before the write: an ancestor may have become a link
		return err
	}
	abs := filepath.Join(repo.Dir, c.Rel)
	if c.Delete {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := paths.AtomicWrite(abs, c.Data, 0o600); err != nil {
		return err
	}
	rec.Applied = append(rec.Applied, c.Rel)
	return nil
}

// recordUnapplied files a skipped or divergent change in rec and reports whether it did.
func (c journalChange) recordUnapplied(o outcome, rec *Receipt) bool {
	switch o {
	case outSkip:
		rec.Skipped = append(rec.Skipped, c.Rel)
	case outDiverge:
		rec.Divergent = append(rec.Divergent, c.Rel)
	default:
		return false
	}
	return true
}

// dirtyFiles lists the dirty paths outside .git that are REGULAR files or now missing (a tracked deletion);
// symlinks and other oddities are skipped and listed nowhere.
func dirtyFiles(repo Repo) (present []string, err error) {
	entries, err := repo.Status()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Path == ".git" || strings.HasPrefix(e.Path, ".git/") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(repo.Dir, e.Path))
		switch {
		case os.IsNotExist(err):
			present = append(present, e.Path)
		case err != nil:
			return nil, err
		case fi.Mode().IsRegular():
			present = append(present, e.Path)
		}
	}
	return present, nil
}

// journalSchema versions memory-journal.
const journalSchema = 1

var journalFormat = schemas.Register(schemas.Format{
	Name: "memory-journal", Owner: "internal/memory", Kind: schemas.Record, Version: journalSchema,
	Glob:    func() string { return filepath.Join(JournalDir(), "*.json") },
	New:     func() any { return new(journal) },
	Keep:    "a pending journal is replayed by the next commit; a newer one is never replayed",
	Migrate: schemas.SameShape,
})
