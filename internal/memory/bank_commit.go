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
func StoreRepo() Repo { return Repo{Dir: StoreDir()} }

// prePushHook refuses every push: a guard against accidents, not an access-control boundary.
const prePushHook = "#!/bin/sh\n# memory is local-only: refuse every push; a guard against accidents, not access control.\n" +
	"echo 'memory: this repository is local-only; push refused' >&2\nexit 1\n"

// storeGitignore keeps runtime beacons beside memory files out of history; locks live under state now.
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
	if remotes, err := r.remotes(); err != nil {
		return err
	} else if len(remotes) > 0 {
		return fmt.Errorf("memory repo is local-only but has remotes: %s", strings.Join(remotes, ", "))
	}
	if err := writeIfAbsent(filepath.Join(gitDir, "hooks", "pre-push"), prePushHook, 0o755); err != nil {
		return err
	}
	return writeIfAbsent(filepath.Join(r.Dir, ".gitignore"), storeGitignore, 0o644)
}

func (r Repo) initIfAbsent(gitDir string) error {
	if r.Exists() {
		return nil
	}
	if _, err := os.Lstat(gitDir); err == nil {
		return fmt.Errorf("memory repo: %s exists but is not a directory (foreign worktree?)", gitDir)
	}
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

// writeIfAbsent creates path with body unless it already exists (a user's own edit is never overwritten).
func writeIfAbsent(path, body string, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), mode)
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
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}
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
	return entries, nil
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
	if err := r.stageRels(rels); err != nil {
		return "", false, err
	}
	if _, err := r.run("diff", "--cached", "--quiet"); err == nil {
		return "", false, nil
	} else if exitCode(err) != 1 {
		return "", false, err
	}
	args := []string{"commit", "--quiet", "-m", subject}
	if body != "" {
		args = append(args, "-m", body)
	}
	if _, err := r.run(args...); err != nil {
		return "", false, err
	}
	sha, err = r.head()
	return sha, true, err
}

func (r Repo) stageRels(rels []string) error {
	stage, err := r.stageable(rels)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(stage, 200) { // keeps the argv under the Windows command-line limit
		if _, err := r.run(append([]string{"add", "-A", "--"}, chunk...)...); err != nil {
			return err
		}
	}
	return r.checkStaged(rels)
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

func checkStageContent(abs, rel string) error {
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	if textutil.CredentialRE.Match(data) {
		return fmt.Errorf("credential-shaped content: %s", rel)
	}
	return nil
}

// dropCredentialShaped drops credential-shaped files that keep does not claim, naming each path once on stderr.
func (r Repo) dropCredentialShaped(rels []string, keep func(rel string) bool) []string {
	var clean []string
	for _, rel := range rels {
		if keep == nil || !keep(rel) {
			if data, err := os.ReadFile(filepath.Join(r.Dir, rel)); err == nil && textutil.CredentialRE.Match(data) {
				fmt.Fprintf(os.Stderr, "memory: skipping credential-shaped file %s (scrub it; it is not committed)\n", rel)
				continue
			}
		}
		clean = append(clean, rel)
	}
	return clean
}

// checkStaged refuses an index holding anything beyond rels: a stray `git add` (or a pathspec glob) skipped validation.
func (r Repo) checkStaged(rels []string) error {
	out, err := r.run("diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(rels))
	for _, rel := range rels {
		want[filepath.ToSlash(rel)] = true
	}
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" && !want[p] {
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
	full := []string{"-C", r.Dir, "--git-dir=" + gitDir, "--work-tree=" + r.Dir, "--literal-pathspecs",
		"-c", "user.name=claude-code", "-c", "user.email=claude-code@localhost",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.excludesFile=" + os.DevNull}
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
