package memory

import (
	"bytes"
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

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// The batch commit: capture native revisions, apply with preimage checks, roll back on a write error, commit

// storeChange is one store-relative write or deletion, checked against its preimage before the write.
type storeChange struct {
	rel    string // store-relative, validated like Commit
	data   []byte // new bytes; ignored when Delete
	delete bool
	// planned makes PreSHA256 ("" = absent) the preimage, so bytes edited since the caller read them diverge.
	planned   bool
	preSHA256 string
}

// commitReceipt records one applied batch; it is returned to the caller and never written.
type commitReceipt struct {
	commit      string
	applied     []string // written or removed by this batch
	skipped     []string // already at Data (or deleted and absent)
	divergent   []string // preimage mismatch; left untouched
	preexisting []string // dirty native paths outside the batch, swept into Commit
}

// errDivergent accompanies a receipt when some checked changes found other bytes; what applied is committed.
var errDivergent = errors.New("memory transaction: live bytes diverged from the preimage")

// changeState is what decide found for one change against the live bytes.
type changeState int

const (
	stApply   changeState = iota // preimage holds: write or remove
	stSkip                       // already at Data, or deleted and absent
	stDiverge                    // other bytes: leave untouched, report
)

// commitChangesLocked applies changes while callers hold the writable bank lock; a failed write rolls the batch back.
func commitChangesLocked(subject string, changes []storeChange) (commitReceipt, error) {
	repo := StoreRepo()
	seen := make(map[string]bool, len(changes))
	for _, c := range changes {
		if seen[c.rel] {
			return commitReceipt{}, fmt.Errorf("memory transaction: duplicate path %s", c.rel)
		}
		seen[c.rel] = true
		if err := repo.validateRel(c.rel); err != nil {
			return commitReceipt{}, err
		}
	}
	if err := repo.Ensure(); err != nil {
		return commitReceipt{}, err
	}
	present, pre, err := dirtyFiles(repo, changes)
	if err != nil {
		return commitReceipt{}, err
	}
	touches := func(rel string) bool { return touchesTop(changes, rel) }
	present = repo.dropCredentialShaped(present, touches) // an unrelated dir's credential file must not wedge this batch
	if err := repo.refuseCredentialShaped(present, touches); err != nil {
		return commitReceipt{}, err
	}
	var rec commitReceipt
	states, initial, err := captureNative(repo, subject, changes, present, pre, &rec)
	if err != nil {
		return rec, err
	}
	if err := writeChanges(repo, changes, states, initial, &rec); err != nil {
		return rec, err
	}
	if stage := commitStage(rec, present); len(stage) > 0 { // a failed commit leaves the files for the next settle
		body := fmt.Sprintf("applied %d, skipped %d, divergent %d, preexisting %d",
			len(rec.applied), len(rec.skipped), len(rec.divergent), len(rec.preexisting))
		sha, committed, err := repo.commit(stage, subject, body)
		if committed {
			rec.commit = sha
		}
		if err != nil {
			return rec, err
		}
	}
	if len(rec.divergent) > 0 {
		return rec, errDivergent
	}
	return rec, nil
}

// touchesTop reports whether rel sits under a top-level dir (or is a root file) the changes touch.
func touchesTop(changes []storeChange, rel string) bool {
	top, _, _ := strings.Cut(rel, "/")
	return slices.ContainsFunc(changes, func(c storeChange) bool {
		t, _, _ := strings.Cut(c.rel, "/")
		return t == top
	})
}

// captureNeedless reports a capture that would stage nothing: index at HEAD, rel clean and stat-stable, no credential.
func captureNeedless(atHead bool, present []string, rel string, pre fs.FileInfo, snap fileSnapshot) bool {
	if !atHead || slices.Contains(present, rel) {
		return false
	}
	now, err := os.Lstat(snap.path)
	if pre == nil || err != nil {
		return pre == nil && !snap.existed && errors.Is(err, fs.ErrNotExist) // absent and untracked throughout
	}
	return !credentialShaped(snap.prev) && os.SameFile(pre, now) && pre.Size() == now.Size() && pre.ModTime().Equal(now.ModTime())
}

// captureNative commits, ahead of the batch, the dirty revisions it replaces; other dirty files are Preexisting.
func captureNative(repo storeRepo, subject string, changes []storeChange, present []string, pre []fs.FileInfo,
	rec *commitReceipt) ([]changeState, []fileSnapshot, error) {
	known, ok := knownStaged(repo)
	atHead := ok && len(known) == 0 // index is HEAD, so a clean file's capture stages nothing
	inBatch := map[string]bool{}
	states := make([]changeState, len(changes))
	initial := make([]fileSnapshot, len(changes))
	var capture []string
	for i, c := range changes {
		var err error
		initial[i], err = snapshotFile(filepath.Join(repo.dir, c.rel))
		if err != nil {
			return nil, nil, err
		}
		st, err := c.decide(repo.dir)
		if err != nil {
			return nil, nil, err
		}
		states[i] = st
		inBatch[c.rel] = true
		if st == stApply && !captureNeedless(atHead, present, c.rel, pre[i], initial[i]) {
			capture = append(capture, c.rel)
		}
	}
	for _, p := range present {
		if !inBatch[p] {
			rec.preexisting = append(rec.preexisting, p)
		}
	}
	if len(capture) == 0 {
		return states, initial, nil
	}
	_, _, err := repo.commit(capture, "native: before "+subject, fmt.Sprintf("captured %d", len(capture)))
	return states, initial, err
}

type appliedWrite struct {
	change storeChange
	snap   fileSnapshot
}

// writeChanges rechecks each change's live bytes, writes the ones that hold, and restores the earlier files on error.
func writeChanges(repo storeRepo, changes []storeChange, states []changeState, initial []fileSnapshot, rec *commitReceipt) error {
	abs := make([]string, len(changes))
	for i, c := range changes {
		abs[i] = filepath.Join(repo.dir, c.rel)
	}
	snaps, err := snapshotPaths(abs...)
	if err != nil {
		return err
	}
	var applied []appliedWrite
	for i, c := range changes {
		before := len(rec.applied)
		if err := c.write(repo, abs[i], states[i], initial[i], rec); err != nil {
			return rollbackWritten(appliedWrite{c, snaps[i]}, applied, rec, err)
		}
		if len(rec.applied) > before {
			applied = append(applied, appliedWrite{c, snaps[i]})
		}
	}
	return nil
}

// restoreIfOurs leaves a native edit alone if it arrived after this batch wrote the file.
func (w appliedWrite) restoreIfOurs() error {
	current, err := snapshotFile(w.snap.path)
	if err != nil {
		return err
	}
	if current.existed == w.snap.existed && bytes.Equal(current.prev, w.snap.prev) {
		return nil
	}
	if w.change.delete && !current.existed || !w.change.delete && current.existed && bytes.Equal(current.prev, w.change.data) {
		return w.snap.restore()
	}
	return fmt.Errorf("memory transaction: %s changed during rollback", w.change.rel)
}

// rollbackWritten restores only this batch's bytes and leaves foreign edits untouched.
func rollbackWritten(failed appliedWrite, applied []appliedWrite, rec *commitReceipt, cause error) error {
	current, err := snapshotFile(failed.snap.path)
	if err == nil && (current.existed != failed.snap.existed || !bytes.Equal(current.prev, failed.snap.prev)) {
		applied = append(applied, failed)
	}
	for _, w := range slices.Backward(applied) {
		err = errors.Join(err, w.restoreIfOurs())
	}
	rec.applied = nil // callers must not restore a native edit after this guard refuses it
	return errors.Join(cause, err)
}

// write sorts one change into Applied, Skipped or Divergent, mutating only when the preimage still holds.
func (c storeChange) write(repo storeRepo, abs string, planned changeState, snap fileSnapshot, rec *commitReceipt) error {
	st, err := c.guardedState(repo.dir, abs, planned, snap)
	if err != nil {
		return err
	}
	switch st {
	case stSkip:
		rec.skipped = append(rec.skipped, c.rel)
		return nil
	case stDiverge:
		rec.divergent = append(rec.divergent, c.rel)
		return nil
	}
	if err := repo.validateRel(c.rel); err != nil { // again, right before the write: an ancestor may have become a link
		return err
	}
	if c.delete {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := paths.AtomicWrite(abs, c.data, 0o600); err != nil {
		return err
	}
	rec.applied = append(rec.applied, c.rel)
	return nil
}

// guardedState rejects a late preimage match or an unplanned change to the captured bytes.
func (c storeChange) guardedState(dir, abs string, planned changeState, snap fileSnapshot) (changeState, error) {
	st, err := c.decide(dir)
	if err != nil || st != stApply {
		return st, err
	}
	if planned != stApply {
		return stDiverge, nil
	}
	if c.planned {
		return stApply, nil
	}
	cur, err := snapshotFile(abs)
	if err != nil {
		return 0, err
	}
	if cur.existed != snap.existed || !bytes.Equal(cur.prev, snap.prev) {
		return stDiverge, nil
	}
	return stApply, nil
}

// decide reads the live bytes once; an unplanned change takes whatever is live as its preimage.
func (c storeChange) decide(dir string) (changeState, error) {
	cur, err := os.ReadFile(filepath.Join(dir, c.rel))
	absent := os.IsNotExist(err)
	if err != nil && !absent {
		return 0, err
	}
	switch {
	case c.delete && absent, !c.delete && !absent && bytes.Equal(cur, c.data):
		return stSkip, nil
	case !c.planned, c.preSHA256 == "" && absent, !absent && textutil.SHA256Hex(cur) == c.preSHA256:
		return stApply, nil
	}
	return stDiverge, nil
}

// commitStage lists the paths the batch commit stages: applied, preexisting, and skipped ones left dirty earlier.
func commitStage(rec commitReceipt, present []string) []string {
	stage := append(append([]string{}, rec.applied...), rec.preexisting...)
	for _, p := range rec.skipped {
		if slices.Contains(present, p) {
			stage = append(stage, p)
		}
	}
	return stage
}

// dirtyFiles lists dirty regular files outside .git or tracked deletions, and pre: each change's Lstat before status.
func dirtyFiles(repo storeRepo, changes []storeChange) (present []string, pre []fs.FileInfo, err error) {
	pre = make([]fs.FileInfo, len(changes))
	for i, c := range changes {
		pre[i], _ = os.Lstat(filepath.Join(repo.dir, c.rel))
	}
	entries, err := repo.status()
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.path == ".git" || strings.HasPrefix(e.path, ".git/") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(repo.dir, e.path))
		switch {
		case os.IsNotExist(err):
			present = append(present, e.path)
		case err != nil:
			return nil, nil, err
		case fi.Mode().IsRegular():
			present = append(present, e.path)
		}
	}
	return present, pre, nil
}

// Bounded git for the nested memory repo: explicit --git-dir/--work-tree keep a stray GIT_DIR off ~/.claude.

// storeRepo is the nested memory repository rooted at Dir (its work tree); .git lives directly inside it.
type storeRepo struct{ dir string }

// StoreRepo is the live memory store's repo.
func StoreRepo() storeRepo { return storeRepo{dir: paths.MemoryStoreDir()} }

// prePushHook refuses every push: a guard against accidents, not an access-control boundary.
const prePushHook = "#!/bin/sh\n# memory is local-only: refuse every push; a guard against accidents, not access control.\n" +
	"echo 'memory: this repository is local-only; push refused' >&2\nexit 1\n"

// storeGitignore keeps runtime beacons out of history.
const storeGitignore = gitignoreHeader + "\n.DS_Store\naccess.log\n*.tmp-*\n"

// gitignoreHeader marks a harness-written .gitignore; a file without it is the user's own.
const gitignoreHeader = "# harness runtime beacons beside memory files — never history"

// exists reports a real nested repo: <Dir>/.git is a directory (a gitdir: pointer file is a foreign worktree).
func (r storeRepo) exists() bool {
	fi, err := os.Lstat(filepath.Join(r.dir, ".git"))
	return err == nil && fi.IsDir()
}

// plainDir creates the repo root and rejects a symlinked one, which could redirect the store into any repo.
func (r storeRepo) plainDir() error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(r.dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() { // a symlinked root Lstats as a link
		return fmt.Errorf("memory repo: %s is not a plain directory (symlink?)", r.dir)
	}
	return nil
}

// Ensure inits the repo when absent and verifies root, distinctness and no remotes BEFORE writing any guard file.
func (r storeRepo) Ensure() error {
	if err := r.plainDir(); err != nil {
		return err
	}
	gitDir := filepath.Join(r.dir, ".git")
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
	if _, err := paths.WriteNoReplace(filepath.Join(gitDir, "hooks", "pre-push"), []byte(prePushHook), 0o755); err != nil {
		return err
	}
	return r.syncGitignore()
}

// syncGitignore seeds .gitignore, and refreshes a stale harness-written copy (header line present).
func (r storeRepo) syncGitignore() error {
	dest := filepath.Join(r.dir, ".gitignore")
	created, err := paths.WriteNoReplace(dest, []byte(storeGitignore), 0o644)
	if err != nil || created {
		return err
	}
	cur, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	first, _, _ := strings.Cut(string(cur), "\n")
	if first != gitignoreHeader {
		return nil
	}
	var want strings.Builder
	want.WriteString(storeGitignore)
	for line := range strings.SplitSeq(string(cur), "\n") { // the user's own lines stay; retired harness ones go
		if line != "" && line != gitignoreHeader && !slices.Contains(retiredGitignore, line) &&
			!strings.Contains("\n"+storeGitignore, "\n"+line+"\n") {
			want.WriteString(line + "\n")
		}
	}
	if string(cur) == want.String() {
		return nil
	}
	return paths.AtomicWrite(dest, []byte(want.String()), 0o644)
}

// retiredGitignore lists lines older binaries wrote under the header; a refresh drops them.
var retiredGitignore = []string{"*.lock", ".postharvest*", ".promote.lock", ".archive*"}

func (r storeRepo) initIfAbsent(gitDir string) error {
	if r.exists() {
		return nil
	}
	if _, err := os.Lstat(gitDir); err == nil {
		return fmt.Errorf("memory repo: %s exists but is not a directory (foreign worktree?)", gitDir)
	}
	return gitInit(r)
}

// gitInit creates r's repository, a var so tests copy a prebuilt one instead of spawning git.
var gitInit = func(r storeRepo) error {
	_, err := r.run("init", "--quiet", "--initial-branch=main")
	return err
}

// verifiedRoots holds canonical git dirs whose checkRoot passed in this process; Ensure skips re-probing them.
var verifiedRoots sync.Map

// checkRoot proves discovery from inside Dir lands on gitDir, with no shared commondir or enclosing checkout.
func (r storeRepo) checkRoot(gitDir string) error {
	out, err := gitOut(r.dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return err
	}
	found, common, _ := strings.Cut(strings.TrimSpace(out), "\n")
	want := paths.CanonDeep(gitDir)
	if paths.CanonDeep(found) != want {
		return fmt.Errorf("memory repo: git in %s resolves to %q, not %q", r.dir, found, gitDir)
	}
	if paths.CanonDeep(common) != want {
		return fmt.Errorf("memory repo: %s shares git metadata with %q (commondir)", gitDir, common)
	}
	outer, err := gitOut(filepath.Dir(r.dir), "rev-parse", "--absolute-git-dir")
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
func (r storeRepo) checkNoRemotes(gitDir, key string) error {
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
func (r storeRepo) remotes() ([]string, error) {
	out, err := r.run("remote")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// statusEntry is one porcelain v1 row: the XY code and the path (a rename reports its NEW path, then OLD as "D ").
type statusEntry struct{ code, path string }

// status lists dirty paths (untracked files individually), limited to pathspecs when given.
func (r storeRepo) status(pathspecs ...string) ([]statusEntry, error) {
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
		entries = append(entries, statusEntry{code: e.Code, path: e.Path})
		if strings.Contains(e.Code, "R") && e.From != "" {
			entries = append(entries, statusEntry{code: "D ", path: e.From}) // a rename also deletes its source
		}
	}
	rememberIndex(r, before, pinned && len(pathspecs) == 0, entries)
	return entries, nil
}

// rememberIndex records what a full status saw staged when the index and HEAD held still across it.
func rememberIndex(r storeRepo, before indexState, full bool, entries []statusEntry) {
	after, ok := indexStamp(r)
	if !full || !ok || !before.same(after) {
		indexMemo.Delete(r.dir)
		return
	}
	for _, e := range entries {
		if x := (e.code + " ")[0]; x != ' ' && x != '?' && x != '!' {
			after.staged = append(after.staged, e.path)
		}
	}
	indexMemo.Store(r.dir, after)
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
func indexStamp(r storeRepo) (indexState, bool) {
	fi, err := os.Stat(filepath.Join(r.dir, ".git", "index"))
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
func knownStaged(r storeRepo) ([]string, bool) {
	v, ok := indexMemo.Load(r.dir)
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
func (r storeRepo) head() (string, error) {
	out, err := r.run("rev-parse", "--verify", "--quiet", "--short", "HEAD")
	if exitCode(err) == 1 {
		return "", nil // --verify --quiet exits 1 only for an unborn HEAD
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// commit stages exactly rels and commits when the index changed; any earlier failure unstages everything.
func (r storeRepo) commit(rels []string, subject, body string) (sha string, committed bool, err error) {
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
func (r storeRepo) commitSHA(out string) string {
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
func (r storeRepo) headSHA() (string, bool) {
	gitDir := filepath.Join(r.dir, ".git")
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
func (r storeRepo) stageRels(rels []string) (bool, error) {
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
func (r storeRepo) stageable(rels []string) ([]string, error) {
	var present, absent []string
	for _, rel := range rels {
		if err := r.validateRel(rel); err != nil {
			return nil, err
		}
		abs := filepath.Join(r.dir, rel)
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
	return append(present, textutil.SplitNUL(out)...), nil
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
func (r storeRepo) dropCredentialShaped(rels []string, keep func(rel string) bool) []string {
	var clean []string
	for _, rel := range rels {
		if keep == nil || !keep(rel) {
			if data, err := os.ReadFile(filepath.Join(r.dir, rel)); err == nil && credentialShaped(data) {
				fmt.Fprintf(os.Stderr, "memory: skipping credential-shaped file %s (scrub it; it is not committed)\n", rel)
				continue
			}
		}
		clean = append(clean, rel)
	}
	return clean
}

// refuseCredentialShaped errors on a credential-shaped file keep claims: it would fail the commit after apply.
func (r storeRepo) refuseCredentialShaped(rels []string, keep func(rel string) bool) error {
	for _, rel := range rels {
		if keep(rel) {
			if data, err := os.ReadFile(filepath.Join(r.dir, rel)); err == nil && credentialShaped(data) {
				return fmt.Errorf("credential-shaped content: %s", rel)
			}
		}
	}
	return nil
}

// checkStaged refuses an index holding anything beyond rels: a stray `git add` (or a pathspec glob) skipped validation.
func (r storeRepo) checkStaged(rels []string) (staged []string, err error) {
	out, err := r.run("diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	staged = textutil.SplitNUL(out)
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
func (r storeRepo) validateRel(rel string) error {
	bad := fmt.Errorf("memory repo: invalid path %q", rel)
	if invalidRelSyntax(rel) {
		return bad
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if strings.EqualFold(parts[0], ".git") || slices.Contains(parts, "..") {
		return bad
	}
	abs := r.dir
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
func (r storeRepo) run(args ...string) (string, error) {
	gitDir := filepath.Join(r.dir, ".git")
	full := append([]string{"-C", r.dir, "--git-dir=" + gitDir, "--work-tree=" + r.dir, "--literal-pathspecs"}, proc.Values{
		"user.name": "claude-code", "user.email": "claude-code@localhost",
		"commit.gpgsign": "false", "tag.gpgsign": "false",
		"core.hooksPath": os.DevNull, "core.excludesFile": os.DevNull}.Args("-c")...)
	return runGit(append(full, args...))
}

// runGit is the one exec seam (internal/proc): env minus GIT_*, bounded, trims only trailing newlines; full carries -C.
func runGit(full []string) (string, error) {
	return proc.GitTimeout(gitTimeout, "", full...)
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
