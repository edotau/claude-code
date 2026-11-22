package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// The journaled commit: journal first, capture native revisions, apply with preimage checks, commit, retire the journal

// Change is one store-relative write or deletion; its journal-time bytes are the preimage, so replay never clobbers.
type Change struct {
	Rel    string // store-relative, validated like Commit
	Data   []byte // new bytes; ignored when Delete
	Delete bool
	// Planned makes PreSHA256 ("" = absent) the preimage, so bytes edited since the caller read them diverge.
	Planned   bool
	PreSHA256 string
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

// journal is the durable pre-write record of a batch; it is removed when the batch completes.
type journal struct {
	Schema  int             `json:"schema"`
	ID      string          `json:"id"`
	Subject string          `json:"subject"`
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

// recoverEach finishes every unfinished journal oldest first; a failing one is skipped and may diverge on replay.
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

// newJournal records preimages as hashes and new bytes verbatim, so replay tells "not applied" from "edited since".
func newJournal(repo Repo, subject string, changes []Change) (*journal, error) {
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	j := &journal{ID: time.Now().UTC().Format("20060102-150405.000") + "-" + hex.EncodeToString(rnd[:]), Subject: subject, Schema: journalSchema}
	for _, c := range changes {
		jc := journalChange{Rel: c.Rel, Delete: c.Delete, HasPreimage: c.PreSHA256 != "", PreimageSHA256: c.PreSHA256}
		if !c.Planned {
			pre, err := os.ReadFile(filepath.Join(repo.Dir, c.Rel))
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			jc.HasPreimage, jc.PreimageSHA256 = pre != nil, ""
			if pre != nil {
				jc.PreimageSHA256 = textutil.SHA256Hex(pre)
			}
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

// run drives one journal: check, native capture, apply, commit, retire; a failed commit leaves the journal for replay.
func (j *journal) run(repo Repo) (Receipt, error) {
	rec := Receipt{ID: j.ID}
	present, err := j.applyBatch(repo, &rec)
	if err != nil {
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
	if err := repo.refuseCredentialShaped(present, j.touches); err != nil {
		return nil, err
	}
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

// commitStage lists the paths the batch commit stages: applied, preexisting, and skipped ones left dirty earlier.
func commitStage(rec Receipt, present []string) []string {
	stage := append(append([]string{}, rec.Applied...), rec.Preexisting...)
	for _, p := range rec.Skipped {
		if slices.Contains(present, p) {
			stage = append(stage, p)
		}
	}
	return stage
}

// plan checks each change against live bytes; dirty files outside the batch are Preexisting, inside it are captured.
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

// apply rechecks live bytes before mutating and sorts the rel into Applied, Skipped or Divergent.
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

// dirtyFiles lists dirty regular files outside .git, or now-missing ones (a tracked deletion); oddities are skipped.
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

// Bounded git for the nested memory repo: explicit --git-dir/--work-tree keep a stray GIT_DIR off ~/.claude.

// Repo is the nested memory repository rooted at Dir (its work tree); .git lives directly inside it.
type Repo struct{ Dir string }

// StoreRepo is the live memory store's repo.
func StoreRepo() Repo { return Repo{Dir: paths.MemoryStoreDir()} }

// prePushHook refuses every push: a guard against accidents, not an access-control boundary.
const prePushHook = "#!/bin/sh\n# memory is local-only: refuse every push; a guard against accidents, not access control.\n" +
	"echo 'memory: this repository is local-only; push refused' >&2\nexit 1\n"

// storeGitignore keeps runtime beacons out of history; .archive* is for kept copies an older binary leaves in the root.
const storeGitignore = "# harness runtime beacons beside memory files — never history\n.archive*\naccess.log\n*.tmp-*\n"

// Exists reports a real nested repo: <Dir>/.git is a directory (a gitdir: pointer file is a foreign worktree).
func (r Repo) Exists() bool {
	fi, err := os.Lstat(filepath.Join(r.Dir, ".git"))
	return err == nil && fi.IsDir()
}

// plainDir creates the repo root and rejects a symlinked one, which could redirect the store into any repo.
func (r Repo) plainDir() error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(r.Dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() { // a symlinked root Lstats as a link
		return fmt.Errorf("memory repo: %s is not a plain directory (symlink?)", r.Dir)
	}
	return nil
}

// Ensure inits the repo when absent and verifies root, distinctness and no remotes BEFORE writing any guard file.
func (r Repo) Ensure() error {
	if err := r.plainDir(); err != nil {
		return err
	}
	gitDir := filepath.Join(r.Dir, ".git")
	if err := r.initIfAbsent(gitDir); err != nil {
		return err
	}
	key := paths.CanonDeep(gitDir)
	if _, ok := verifiedRoots.Load(key); !ok {
		if err := r.checkRoot(gitDir); err != nil {
			return err
		}
		verifiedRoots.Store(key, struct{}{})
	}
	if err := r.checkNoRemotes(gitDir, key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0o755); err != nil {
		return err
	}
	if err := createExcl(filepath.Join(gitDir, "hooks", "pre-push"), prePushHook, 0o755); err != nil {
		return err
	}
	return createExcl(filepath.Join(r.Dir, ".gitignore"), storeGitignore, 0o644)
}

func (r Repo) initIfAbsent(gitDir string) error {
	if r.Exists() {
		return nil
	}
	if _, err := os.Lstat(gitDir); err == nil {
		return fmt.Errorf("memory repo: %s exists but is not a directory (foreign worktree?)", gitDir)
	}
	return gitInit(r)
}

// gitInit creates r's repository, a var so tests copy a prebuilt one instead of spawning git.
var gitInit = func(r Repo) error {
	_, err := r.run("init", "--quiet", "--initial-branch=main")
	return err
}

// verifiedRoots holds canonical git dirs whose checkRoot passed in this process; Ensure skips re-probing them.
var verifiedRoots sync.Map

// checkRoot proves discovery from inside Dir lands on gitDir, with no shared commondir or enclosing checkout.
func (r Repo) checkRoot(gitDir string) error {
	out, err := gitOut(r.Dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return err
	}
	found, common, _ := strings.Cut(strings.TrimSpace(out), "\n")
	want := paths.CanonDeep(gitDir)
	if paths.CanonDeep(found) != want {
		return fmt.Errorf("memory repo: git in %s resolves to %q, not %q", r.Dir, found, gitDir)
	}
	if paths.CanonDeep(common) != want {
		return fmt.Errorf("memory repo: %s shares git metadata with %q (commondir)", gitDir, common)
	}
	outer, err := gitOut(filepath.Dir(r.Dir), "rev-parse", "--absolute-git-dir")
	if err == nil && paths.CanonDeep(outer) == want {
		return fmt.Errorf("memory repo: %s is the outer checkout's git dir", gitDir)
	}
	return nil
}

// remoteFree maps a canonical git dir to the config stamp under which `git remote` last listed nothing.
var remoteFree sync.Map

// configStamp fingerprints the repo and user git config files by size and mtime; "" when none can be stat'ed.
func configStamp(gitDir string) string {
	var b strings.Builder
	files := []string{filepath.Join(gitDir, "config")}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".gitconfig"))
	}
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", f, fi.Size(), fi.ModTime().UnixNano())
		} else {
			textutil.WriteStrings(&b, f, ":-;")
		}
	}
	return b.String()
}

// checkNoRemotes refuses a store with remotes; an unchanged config since the last clean listing skips the git call.
func (r Repo) checkNoRemotes(gitDir, key string) error {
	stamp := configStamp(gitDir)
	if v, ok := remoteFree.Load(key); ok && v.(string) == stamp {
		return nil
	}
	remotes, err := r.remotes()
	if err != nil {
		return err
	}
	if len(remotes) > 0 {
		return fmt.Errorf("memory repo is local-only but has remotes: %s", strings.Join(remotes, ", "))
	}
	remoteFree.Store(key, stamp)
	return nil
}

// remotes lists configured remote names; the store must have none.
func (r Repo) remotes() ([]string, error) {
	out, err := r.run("remote")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// statusEntry is one porcelain v1 row: the XY code and the path (a rename reports its NEW path, then OLD as "D ").
type statusEntry struct{ Code, Path string }

// Status lists dirty paths (untracked files individually), limited to pathspecs when given.
func (r Repo) Status(pathspecs ...string) ([]statusEntry, error) {
	before, pinned := indexStamp(r)
	// No optional locks: status must not rewrite the index, so an unchanged stamp proves nobody else did.
	args := []string{"--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all"}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	out, err := r.run(args...)
	if err != nil {
		return nil, err
	}
	var entries []statusEntry
	for _, e := range proc.StatusEntries(out) {
		entries = append(entries, statusEntry{Code: e.Code, Path: e.Path})
		if strings.Contains(e.Code, "R") && e.From != "" {
			entries = append(entries, statusEntry{Code: "D ", Path: e.From}) // a rename also deletes its source
		}
	}
	rememberIndex(r, before, pinned && len(pathspecs) == 0, entries)
	return entries, nil
}

// rememberIndex records what a full status saw staged when the index and HEAD held still across it.
func rememberIndex(r Repo, before indexState, full bool, entries []statusEntry) {
	after, ok := indexStamp(r)
	if !full || !ok || !before.same(after) {
		indexMemo.Delete(r.Dir)
		return
	}
	for _, e := range entries {
		if x := (e.Code + " ")[0]; x != ' ' && x != '?' && x != '!' {
			after.staged = append(after.staged, e.Path)
		}
	}
	indexMemo.Store(r.Dir, after)
}

// indexMemo maps a repo dir to the index-vs-HEAD state its last full Status saw, so Commit can skip `diff --cached`.
var indexMemo sync.Map

// indexState pins the index file and HEAD id a status ran against, and the paths staged against HEAD then.
type indexState struct {
	fi     fs.FileInfo
	head   string
	staged []string
}

// indexStamp reads the index file's identity and HEAD's id; ok is false when either cannot be pinned without git.
func indexStamp(r Repo) (indexState, bool) {
	fi, err := os.Stat(filepath.Join(r.Dir, ".git", "index"))
	if err != nil {
		return indexState{}, false
	}
	head, ok := r.headSHA()
	return indexState{fi: fi, head: head}, ok
}

// same reports one index file (git replaces it by rename on every write) at one size and mtime, under one HEAD.
func (s indexState) same(o indexState) bool {
	return s.head == o.head && s.fi.Size() == o.fi.Size() && s.fi.ModTime().Equal(o.fi.ModTime()) && os.SameFile(s.fi, o.fi)
}

// knownStaged is what the last full Status saw staged, when the index and HEAD are still exactly what it saw.
func knownStaged(r Repo) ([]string, bool) {
	v, ok := indexMemo.Load(r.Dir)
	if !ok {
		return nil, false
	}
	memo := v.(indexState)
	now, ok := indexStamp(r)
	if !ok || !memo.same(now) {
		return nil, false
	}
	return memo.staged, true
}

// head is the short sha of HEAD, "" when the repo has no commits yet.
func (r Repo) head() (string, error) {
	out, err := r.run("rev-parse", "--verify", "--quiet", "--short", "HEAD")
	if exitCode(err) == 1 {
		return "", nil // --verify --quiet exits 1 only for an unborn HEAD
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// Commit stages exactly rels and commits when the index changed; any earlier failure unstages everything.
func (r Repo) Commit(rels []string, subject, body string) (sha string, committed bool, err error) {
	defer func() {
		if err != nil && !committed {
			_, _ = r.run("reset", "--quiet") // a stuck index would fail every later commit as "unexpected staged change"
		}
	}()
	staged, err := r.stageRels(rels)
	if err != nil {
		return "", false, err
	}
	if !staged {
		return "", false, nil
	}
	args := []string{"commit", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	out, err := r.run(args...)
	if err != nil {
		return "", false, err
	}
	if sha = r.commitSHA(out); sha != "" {
		return sha, true, nil
	}
	sha, err = r.head()
	return sha, true, err
}

// commitSHA reads the short id from commit's "[branch sha] subject" line; "" unless HEAD's id starts with it.
func (r Repo) commitSHA(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	inner, _, ok := strings.Cut(strings.TrimPrefix(line, "["), "]")
	if !ok || !strings.HasPrefix(line, "[") {
		return ""
	}
	f := strings.Fields(inner)
	if len(f) == 0 {
		return ""
	}
	sha := f[len(f)-1]
	if full, ok := r.headSHA(); !ok || len(sha) < 4 || !strings.HasPrefix(full, sha) {
		return ""
	}
	return sha
}

// headSHA is HEAD's full id read from the loose or packed ref; ok is false when only git can answer (unborn, odd refs).
func (r Repo) headSHA() (string, bool) {
	gitDir := filepath.Join(r.Dir, ".git")
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(b))
	ref, symbolic := strings.CutPrefix(head, "ref: ")
	if !symbolic {
		return head, isHexID(head)
	}
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return "", false
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		id := strings.TrimSpace(string(b))
		return id, isHexID(id)
	}
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		return "", false
	}
	for line := range strings.Lines(string(packed)) {
		if id, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == ref {
			return id, isHexID(id)
		}
	}
	return "", false
}

// isHexID reports a full SHA-1 or SHA-256 object id.
func isHexID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// stageRels stages rels and reports whether the index now differs from HEAD.
func (r Repo) stageRels(rels []string) (bool, error) {
	stage, err := r.stageable(rels)
	if err != nil {
		return false, err
	}
	known, ok := knownStaged(r)
	if ok {
		if err := refuseUnexpected(known, rels); err != nil {
			return false, err
		}
	}
	added := false
	for chunk := range slices.Chunk(stage, 200) { // keeps the argv under the Windows command-line limit
		out, err := r.run(append([]string{"add", "-A", "-v", "--"}, chunk...)...)
		if err != nil {
			return false, err
		}
		added = added || out != ""
	}
	if ok && len(known) == 0 { // index was HEAD, so `add -v` naming a change is exactly what diff --cached would list
		return added, nil
	}
	staged, err := r.checkStaged(rels)
	return len(staged) > 0, err
}

// stageable validates rels and drops the absent-and-untracked ones (`add` rejects a pathspec matching nothing).
func (r Repo) stageable(rels []string) ([]string, error) {
	var present, absent []string
	for _, rel := range rels {
		if err := r.validateRel(rel); err != nil {
			return nil, err
		}
		abs := filepath.Join(r.Dir, rel)
		if _, err := os.Lstat(abs); err != nil {
			absent = append(absent, rel)
			continue
		}
		if err := checkStageContent(abs, rel); err != nil {
			return nil, err
		}
		present = append(present, rel)
	}
	if len(absent) == 0 {
		return present, nil
	}
	out, err := r.run(append([]string{"ls-files", "-z", "--"}, absent...)...)
	if err != nil {
		return nil, err
	}
	for tracked := range strings.SplitSeq(out, "\x00") {
		if tracked != "" {
			present = append(present, tracked)
		}
	}
	return present, nil
}

// credentialShaped is CredentialRE().Match(data), run anchored at each lead literal instead of scanning every byte.
func credentialShaped(data []byte) bool {
	c := credentialScan()
	if c.lits == nil {
		return textutil.CredentialRE().Match(data)
	}
	for _, lit := range c.lits {
		for i := 0; ; {
			n := bytes.Index(data[i:], lit)
			if n < 0 {
				break
			}
			at := i + n
			// The byte before at rides along so a leading \b sees the same context as a whole-text scan.
			if at == 0 && c.atStart.Match(data) || at > 0 && c.afterByte.Match(data[at-1:]) {
				return true
			}
			i = at + 1
		}
	}
	return false
}

// credentialScanner holds the literals one of which starts every CredentialRE match, and its anchored forms.
type credentialScanner struct {
	lits               [][]byte // nil: some match has no literal start, so callers scan the whole text
	atStart, afterByte *regexp.Regexp
}

var credentialScan = sync.OnceValue(func() credentialScanner {
	pat := textutil.CredentialRE().String()
	c := credentialScanner{
		atStart:   regexp.MustCompile(`\A(?:` + pat + `)`),
		afterByte: regexp.MustCompile(`\A(?s:.)(?:` + pat + `)`),
	}
	re, err := syntax.Parse(pat, syntax.Perl)
	if err != nil {
		return c
	}
	lits, _ := leadLiterals(re)
	for _, l := range lits {
		if l == "" {
			return credentialScanner{}
		}
		c.lits = append(c.lits, []byte(l))
	}
	return c
})

// leadLiterals returns strings one of which starts every match of r, and whether r matches exactly those strings.
func leadLiterals(r *syntax.Regexp) ([]string, bool) {
	switch r.Op {
	case syntax.OpLiteral:
		if r.Flags&syntax.FoldCase != 0 {
			return []string{""}, false
		}
		return []string{string(r.Rune)}, true
	case syntax.OpEmptyMatch, syntax.OpWordBoundary, syntax.OpBeginText, syntax.OpBeginLine:
		return []string{""}, true
	case syntax.OpCapture:
		return leadLiterals(r.Sub[0])
	case syntax.OpAlternate:
		var out []string
		exact := true
		for _, sub := range r.Sub {
			l, e := leadLiterals(sub)
			out, exact = append(out, l...), exact && e
		}
		return out, exact
	case syntax.OpConcat:
		return concatLiterals(r.Sub)
	}
	return []string{""}, false
}

// concatLiterals extends the lead literals through subs while each one matches exactly its literals.
func concatLiterals(subs []*syntax.Regexp) ([]string, bool) {
	out := []string{""}
	for _, sub := range subs {
		l, exact := leadLiterals(sub)
		var next []string
		for _, a := range out {
			for _, b := range l {
				next = append(next, a+b)
			}
		}
		if out = next; !exact {
			return out, false
		}
	}
	return out, true
}

func checkStageContent(abs, rel string) error {
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	if credentialShaped(data) {
		return fmt.Errorf("credential-shaped content: %s", rel)
	}
	return nil
}

// dropCredentialShaped drops credential-shaped files that keep does not claim, naming each path once on stderr.
func (r Repo) dropCredentialShaped(rels []string, keep func(rel string) bool) []string {
	var clean []string
	for _, rel := range rels {
		if keep == nil || !keep(rel) {
			if data, err := os.ReadFile(filepath.Join(r.Dir, rel)); err == nil && credentialShaped(data) {
				fmt.Fprintf(os.Stderr, "memory: skipping credential-shaped file %s (scrub it; it is not committed)\n", rel)
				continue
			}
		}
		clean = append(clean, rel)
	}
	return clean
}

// refuseCredentialShaped errors on a credential-shaped file keep claims: it would fail the commit after apply.
func (r Repo) refuseCredentialShaped(rels []string, keep func(rel string) bool) error {
	for _, rel := range rels {
		if keep(rel) {
			if data, err := os.ReadFile(filepath.Join(r.Dir, rel)); err == nil && credentialShaped(data) {
				return fmt.Errorf("credential-shaped content: %s", rel)
			}
		}
	}
	return nil
}

// checkStaged refuses an index holding anything beyond rels: a stray `git add` (or a pathspec glob) skipped validation.
func (r Repo) checkStaged(rels []string) (staged []string, err error) {
	out, err := r.run("diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" {
			staged = append(staged, p)
		}
	}
	return staged, refuseUnexpected(staged, rels)
}

// refuseUnexpected errors on the first staged path that is not one of rels.
func refuseUnexpected(staged, rels []string) error {
	want := make(map[string]bool, len(rels))
	for _, rel := range rels {
		want[filepath.ToSlash(rel)] = true
	}
	for _, p := range staged {
		if !want[p] {
			return fmt.Errorf("memory repo: unexpected staged change: %s", p)
		}
	}
	return nil
}

// validateRel rejects any rel that is not a plain store-relative path to a regular (or absent) file.
func (r Repo) validateRel(rel string) error {
	bad := fmt.Errorf("memory repo: invalid path %q", rel)
	if invalidRelSyntax(rel) {
		return bad
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if strings.EqualFold(parts[0], ".git") || slices.Contains(parts, "..") {
		return bad
	}
	abs := r.Dir
	for i, p := range parts {
		abs = filepath.Join(abs, p)
		if err := checkRelPart(abs, rel, parts, i); err != nil {
			return err
		}
	}
	return nil
}

// invalidRelSyntax judges rel in slash form: git reports "a/b.md" on every OS, so Windows must not demand "a\\b.md".
func invalidRelSyntax(rel string) bool {
	s := filepath.ToSlash(rel)
	return s == "" || s == "." || s == ".." || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || path.IsAbs(s) ||
		path.Clean(s) != s
}

// checkRelPart requires the i-th path element at abs to be absent, a plain directory, or (the leaf) a regular file.
func checkRelPart(abs, rel string, parts []string, i int) error {
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil // absent: the write creates plain directories
	}
	if leaf := i == len(parts)-1; leaf && !fi.Mode().IsRegular() {
		return fmt.Errorf("memory repo: not a regular file: %s", rel)
	} else if !leaf && !fi.IsDir() {
		return fmt.Errorf("memory repo: not a plain directory: %s", strings.Join(parts[:i+1], "/"))
	}
	return nil
}

// run executes git with explicit dirs, literal pathspecs, a throwaway identity, no signing, no hooks, no global ignore.
func (r Repo) run(args ...string) (string, error) {
	gitDir := filepath.Join(r.Dir, ".git")
	full := append([]string{"-C", r.Dir, "--git-dir=" + gitDir, "--work-tree=" + r.Dir, "--literal-pathspecs"}, proc.Values{
		"user.name": "claude-code", "user.email": "claude-code@localhost",
		"commit.gpgsign": "false", "tag.gpgsign": "false",
		"core.hooksPath": os.DevNull, "core.excludesFile": os.DevNull}.Args("-c")...)
	return runGit(append(full, args...))
}

// runGit is the one exec seam (internal/proc): env minus GIT_*, bounded, trims only trailing newlines; full carries -C.
func runGit(full []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", full...)
}

// exitCode is the git process exit status wrapped in err, -1 when err is not an exit error.
func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

// splitLines splits trimmed output into non-empty lines.
func splitLines(out string) []string {
	var lines []string
	for l := range strings.SplitSeq(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
