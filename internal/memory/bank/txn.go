package bank

// The journaled commit transaction (plan §3.1): journal under state BEFORE any write, capture the native
// revisions the batch replaces, apply with preimage checks, write a tracked receipt, commit, then retire the
// journal. A crash at any step is finished by the next call. Phase 2 adds rotation, bank regions and the catalog.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Change is one store-relative write or deletion; Checked makes it conditional on Preimage (nil = must not exist).
// Unchecked, the bytes present at journal time become the preimage, so a replay never clobbers a later edit.
type Change struct {
	Rel      string // store-relative, validated like Commit
	Data     []byte // new bytes; ignored when Delete
	Delete   bool
	Preimage []byte // expected current bytes; with Checked, nil means "must not exist"
	Checked  bool   // false = fresh import: whatever is there at journal time is the preimage
}

// Receipt records one applied batch; its copy under receipts/ is tracked, as a record only (no cursor recovery reads it).
type Receipt struct {
	ID           string    `json:"id"`
	Subject      string    `json:"subject"`
	Time         time.Time `json:"time"`
	Commit       string    `json:"commit,omitempty"`
	NativeCommit string    `json:"nativeCommit,omitempty"` // holds Captured and Tombstones, made before apply
	Applied      []string  `json:"applied,omitempty"`      // written or removed by this batch
	Skipped      []string  `json:"skipped,omitempty"`      // already at Data (or deleted and absent)
	Divergent    []string  `json:"divergent,omitempty"`    // preimage mismatch; left untouched
	Preexisting  []string  `json:"preexisting,omitempty"`  // dirty native paths outside the batch, swept into Commit
	Captured     []string  `json:"captured,omitempty"`     // dirty native revisions this batch replaced
	Tombstones   []string  `json:"tombstones,omitempty"`   // tracked facts found deleted; HEAD bytes under tombstoneDir
}

// tombstoneDir is where a deleted tracked fact's last committed bytes are copied: <tombstoneDir>/<journal id>/<rel>.
const tombstoneDir = "archive/tombstones"

// ErrDivergent accompanies a receipt when some checked changes found other bytes; what applied is committed.
var ErrDivergent = errors.New("memory transaction: live bytes diverged from the journaled preimage")

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

// CommitChangesLocked applies changes while callers hold the writable bank lock.
func CommitChangesLocked(subject string, changes []Change) (Receipt, error) {
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

// recoverJournals finishes every unfinished journal and returns the scan error and per-journal failures joined.
func recoverJournals() error {
	skipped, err := recoverEach()
	return errors.Join(err, skipped)
}

// recoverEach finishes every unfinished journal, oldest first, each as its own commit; a failing one is left on
// disk (skipped) so later ones still run, and a skipped one may replay after newer commits, where its preimage
// hashes make it diverge instead of clobber. err is a failed directory scan; skipped joins the per-journal failures.
func recoverEach() (skipped, err error) {
	entries, err := os.ReadDir(memstore.JournalDir())
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
		if err := recoverJournal(filepath.Join(memstore.JournalDir(), e.Name())); err != nil {
			skipped = errors.Join(skipped, err)
		}
	}
	return skipped, nil
}

// recoverJournal replays the journal in file f, or just retires it when already complete.
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
	if j.State == "complete" {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if _, err := j.run(StoreRepo()); err != nil && !errors.Is(err, ErrDivergent) {
		return err
	}
	return nil
}

// newJournal records the batch: preimages as hashes, new bytes verbatim, so replay needs nothing from the caller.
// An unchecked change hashes the live bytes (none when absent): replay then tells "not applied" from "edited since".
func newJournal(repo Repo, subject string, changes []Change) (*journal, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	j := &journal{ID: time.Now().UTC().Format("20060102-150405.000") + "-" + hex.EncodeToString(rnd[:]), Subject: subject, State: "pending", Schema: journalSchema}
	for _, c := range changes {
		pre := c.Preimage
		if !c.Checked {
			cur, err := os.ReadFile(filepath.Join(repo.Dir, c.Rel))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			pre = cur // nil when absent
		}
		jc := journalChange{Rel: c.Rel, Delete: c.Delete, HasPreimage: pre != nil}
		if pre != nil {
			jc.PreimageSHA256 = sha256Hex(pre)
		}
		if !c.Delete {
			jc.Data = c.Data
		}
		j.Changes = append(j.Changes, jc)
	}
	return j, nil
}

func (j *journal) path() string { return filepath.Join(memstore.JournalDir(), j.ID+".json") }

// save writes the journal atomically under state; it is never committed.
func (j *journal) save() error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(memstore.JournalDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(j.path(), data, 0o600)
}

// run drives one journal: check, native capture, apply, receipt, commit, complete; a failed commit stays "applied".
func (j *journal) run(repo Repo) (Receipt, error) {
	rec := Receipt{ID: j.ID, Subject: j.Subject, Time: time.Now().UTC()}
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
		return rec, ErrDivergent
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
	if err := j.discardReceipt(repo); err != nil {
		return nil, err
	}
	present, missing, err := dirtyFiles(repo)
	if err != nil {
		return nil, err
	}
	outcomes, capture, err := j.plan(repo.Dir, present, rec)
	if err != nil {
		return nil, err
	}
	if err := j.captureNative(repo, rec, capture, missing); err != nil {
		return nil, err
	}
	for i, c := range j.Changes {
		if err := c.apply(repo, outcomes[i], rec); err != nil {
			return nil, err
		}
	}
	return present, nil
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

// captureNative commits, ahead of the batch, the dirty revisions it replaces plus a recovery copy of every tracked
// fact found deleted (its deletion rides along, explained by the receipt's Tombstones rather than swept silently).
func (j *journal) captureNative(repo Repo, rec *Receipt, capture, missing []string) error {
	stage := append([]string{}, capture...)
	for _, p := range missing {
		// Never committed (a staged add that did not land) has no HEAD bytes to copy.
		tracked, err := trackedAtHead(repo, p)
		if err != nil {
			return fmt.Errorf("recovery copy of %s: %w", p, err)
		}
		if tracked {
			data, err := headBytes(repo, p)
			if err != nil {
				return fmt.Errorf("recovery copy of %s: %w", p, err)
			}
			copyRel := path.Join(tombstoneDir, j.ID, p)
			if err := repo.validateRel(copyRel); err != nil {
				return err
			}
			if err := paths.AtomicWrite(filepath.Join(repo.Dir, filepath.FromSlash(copyRel)), data, 0o600); err != nil {
				return err
			}
			stage = append(stage, copyRel)
		}
		stage = append(stage, p)
		rec.Tombstones = append(rec.Tombstones, p)
	}
	if len(stage) == 0 {
		return nil
	}
	rec.Captured = capture
	body := fmt.Sprintf("captured %d, tombstones %d", len(capture), len(rec.Tombstones))
	sha, committed, err := repo.Commit(stage, "native: before "+j.Subject, body)
	if err != nil {
		return err
	}
	if committed {
		rec.NativeCommit = sha
	}
	return nil
}

// receiptRel is this journal's receipt, store-relative.
func (j *journal) receiptRel() string { return "receipts/" + j.ID + ".json" }

// discardReceipt drops the uncommitted receipt a failed earlier attempt left staged or on disk; commit rewrites it.
// A receipt already in HEAD is history and stays.
func (j *journal) discardReceipt(repo Repo) error {
	rel := j.receiptRel()
	tracked, err := trackedAtHead(repo, rel)
	if err != nil || tracked {
		return err
	}
	if _, err := repo.run("rm", "--cached", "--quiet", "--force", "--ignore-unmatch", "--", rel); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(repo.Dir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// commit writes the receipt beside the batch and commits both; the receipt is dropped if nothing was committed.
func (j *journal) commit(repo Repo, rec *Receipt, stage []string) error {
	receiptRel := j.receiptRel()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := repo.validateRel(receiptRel); err != nil {
		return err
	}
	if err := paths.AtomicWrite(filepath.Join(repo.Dir, receiptRel), append(data, '\n'), 0o600); err != nil {
		return err
	}
	body := fmt.Sprintf("applied %d, skipped %d, divergent %d, preexisting %d, captured %d, tombstones %d",
		len(rec.Applied), len(rec.Skipped), len(rec.Divergent), len(rec.Preexisting), len(rec.Captured), len(rec.Tombstones))
	sha, committed, err := repo.Commit(append(stage, receiptRel), j.Subject, body)
	if err != nil {
		return err
	}
	if !committed {
		if err := os.Remove(filepath.Join(repo.Dir, receiptRel)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	rec.Commit = sha
	return nil
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
	return !c.HasPreimage && absent || c.HasPreimage && !absent && sha256Hex(cur) == c.PreimageSHA256
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

// dirtyFiles splits the dirty paths outside .git into present REGULAR files and tracked paths now missing;
// symlinks and other oddities are skipped and listed nowhere.
func dirtyFiles(repo Repo) (present, missing []string, err error) {
	entries, err := repo.Status()
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.Path == ".git" || strings.HasPrefix(e.Path, ".git/") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(repo.Dir, e.Path))
		switch {
		case os.IsNotExist(err):
			missing = append(missing, e.Path)
		case err != nil:
			return nil, nil, err
		case fi.Mode().IsRegular():
			present = append(present, e.Path)
		}
	}
	return present, missing, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// journalSchema versions memory-journal.
const journalSchema = 1

var journalFormat = schemas.Register(schemas.Format{
	Name: "memory-journal", Owner: "internal/memory", Kind: schemas.Record, Version: journalSchema,
	Glob:    func() string { return filepath.Join(memstore.JournalDir(), "*.json") },
	New:     func() any { return new(journal) },
	Keep:    "a pending journal is replayed by the next commit; a newer one is never replayed",
	Migrate: schemas.SameShape,
})
