package memory

// The journaled commit transaction (plan §3.1): journal under state BEFORE any write, capture the native
// revisions the batch replaces, apply with preimage checks, write a tracked receipt, commit, then retire the
// journal. A crash at any step is finished by the next call. Phase 2 adds rotation, bank regions and the catalog.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

// Receipt records one applied batch; the copy under receipts/ is tracked and is the authority for cursor recovery.
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

// CommitChanges applies changes under the guarded bank lock; the writable check runs again once the lock is held.
func CommitChanges(subject string, changes []Change) (Receipt, error) {
	var rec Receipt
	err := WithWritableBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked(subject, changes)
		return err
	})
	return rec, err
}

// commitChangesLocked is CommitChanges without the lock or writable check; callers (migration) already hold the bank lock.
func commitChangesLocked(subject string, changes []Change) (Receipt, error) {
	if err := recoverJournals(); err != nil {
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
	return j.run(repo)
}

// recoverJournals finishes every unfinished journal, oldest first, each as its own commit.
func recoverJournals() error {
	files, _ := filepath.Glob(filepath.Join(JournalDir(), "*.json"))
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var j journal
		if err := json.Unmarshal(data, &j); err != nil {
			return fmt.Errorf("memory journal %s: %w", f, err)
		}
		if j.ID == "" || j.path() != f { // the body's id names where save/retire write: it must be this file
			return fmt.Errorf("memory journal %s: id %q does not match its file name", f, j.ID)
		}
		if j.State == "complete" {
			os.Remove(f)
			continue
		}
		if _, err := j.run(StoreRepo()); err != nil && !errors.Is(err, ErrDivergent) {
			return err
		}
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
	j := &journal{ID: time.Now().UTC().Format("20060102-150405.000") + "-" + hex.EncodeToString(rnd[:]), Subject: subject, State: "pending"}
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

// run drives one journal: check, native capture, apply, receipt, commit, complete; a failed commit stays "applied".
func (j *journal) run(repo Repo) (Receipt, error) {
	rec := Receipt{ID: j.ID, Subject: j.Subject, Time: time.Now().UTC()}
	for _, c := range j.Changes { // a replayed journal never passed CommitChanges' check: nothing is read before this
		if err := repo.validateRel(c.Rel); err != nil {
			return rec, err
		}
	}
	if err := repo.Ensure(); err != nil {
		return rec, err
	}
	present, missing, err := dirtyFiles(repo)
	if err != nil {
		return rec, err
	}
	outcomes, capture, err := j.plan(repo.Dir, present, &rec)
	if err != nil {
		return rec, err
	}
	if err := j.captureNative(repo, &rec, capture, missing); err != nil {
		return rec, err
	}
	for i, c := range j.Changes {
		if err := c.apply(repo, outcomes[i], &rec); err != nil {
			return rec, err
		}
	}
	j.State = "applied"
	if err := j.save(); err != nil {
		return rec, err
	}
	// A skipped rel that is dirty was written by an earlier attempt of this journal: commit it, no duplicate record.
	stage := append(append([]string{}, rec.Applied...), rec.Preexisting...)
	for _, p := range rec.Skipped {
		if slices.Contains(present, p) {
			stage = append(stage, p)
		}
	}
	if len(stage) > 0 {
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
		// Not at HEAD (an add that never committed) leaves nothing to copy; the tombstone still records the loss.
		if data, err := headBytes(repo, p); err == nil {
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

// headBytes is rel's content at HEAD, byte-exact: run trims trailing newlines, which would corrupt a recovery copy.
func headBytes(repo Repo, rel string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+filepath.Join(repo.Dir, ".git"), "--work-tree="+repo.Dir,
		"cat-file", "blob", "HEAD:"+rel)
	cmd.Env = gitEnv()
	return cmd.Output()
}

// commit writes the receipt beside the batch and commits both; the receipt is dropped if nothing was committed.
func (j *journal) commit(repo Repo, rec *Receipt, stage []string) error {
	receiptRel := "receipts/" + j.ID + ".json"
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
		os.Remove(filepath.Join(repo.Dir, receiptRel))
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
	case c.Delete && absent, !c.Delete && !absent && bytes.Equal(cur, c.Data):
		return outSkip, nil
	case !c.HasPreimage && absent, c.HasPreimage && !absent && sha256Hex(cur) == c.PreimageSHA256:
		return outApply, nil
	}
	return outDiverge, nil
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
	switch o {
	case outSkip:
		rec.Skipped = append(rec.Skipped, c.Rel)
		return nil
	case outDiverge:
		rec.Divergent = append(rec.Divergent, c.Rel)
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
