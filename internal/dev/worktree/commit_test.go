package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/shell"
)

var commitRepoOnce sync.Once

// commitRepo is a repo with a.txt, b.txt and c.txt committed, a hermetic config dir, and the caller's session "me".
func commitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(proc.DispatchDepthEnv, "")
	tmpl := filepath.Join(ensureFixtureRoot(t), "commit-repo")
	commitRepoOnce.Do(func() {
		hosttest.RepoAt(t, tmpl)
		for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
			hosttest.WriteRel(t, tmpl, f, "base\n")
		}
		hosttest.In(t, tmpl, "add", ".")
		hosttest.In(t, tmpl, "commit", "-q", "-m", "chore: base")
	})
	dir := filepath.Join(paths.Real(t.TempDir()), fmt.Sprintf("repo%d", fixtureSeq.Add(1)))
	copyFixtureTree(t, tmpl, dir)
	return dir
}

func commitReq(dir, msg string, files ...string) CommitRequest {
	return CommitRequest{Dir: dir, Message: msg, Paths: files, Out: &bytes.Buffer{}, Source: ClaimSource{
		Alive: func(int) bool { return true }, Self: "me", Owner: func() int { return 0 },
		Label: func(pid int) string { return fmt.Sprintf("session peer (pid %d)", pid) }}}
}

// commitRun runs Commit and returns its printed output and error.
func commitRun(req CommitRequest) (string, error) {
	err := Commit(context.Background(), req)
	return req.Out.(*bytes.Buffer).String(), err
}

func commitFiles(t *testing.T, dir string) string {
	t.Helper()
	return hosttest.In(t, dir, "show", "--name-status", "--format=", "HEAD")
}

// peerPid is the owner pid of the fake claims files.
const peerPid = 4242

func claim(t *testing.T, slug, path string) { claimAs(t, slug, "", path) }

// claimAs appends a claim of path by agent (empty: the main session) to the claims file of session slug.
func claimAs(t *testing.T, slug, agent, path string) {
	t.Helper()
	claimAt(t, slug, agent, path, time.Now().Unix())
}

// claimAt is claimAs with an explicit claim time.
func claimAt(t *testing.T, slug, agent, path string, ts int64) {
	t.Helper()
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"schema":1,"path":%q,"agent_id":%q,"ts":%d}`+"\n", path, agent, ts)
	f, err := os.OpenFile(filepath.Join(ClaimsDir(), fmt.Sprintf("%d-%s.jsonl", peerPid, slug)), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, code int) {
	t.Helper()
	var ce *CommitError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("error = %v, want a CommitError with code %d", err, code)
	}
}

func TestCommitExplicitPaths(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "one\ntwo\n")
	hosttest.WriteRel(t, dir, "b.txt", "other\n")
	out, err := commitRun(commitReq(dir, "fix: a", "a.txt"))
	if err != nil || !strings.Contains(out, "a.txt  +2 −1") || !strings.Contains(out, "] fix: a") || !strings.Contains(out, "left out: 1") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if strings.Contains(out, "1 file changed") {
		t.Errorf("shortstat repeats the per-file counts: %q", out)
	}
	if got := commitFiles(t, dir); strings.TrimSpace(got) != "M\ta.txt" {
		t.Fatalf("commit holds %q, want only a.txt", got)
	}
	_, err = commitRun(commitReq(dir, "fix: a", "a.txt"))
	wantCode(t, err, 1)
}

func TestCommitClaimsOnlyLeavesUnclaimedFileOut(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "mine\n")
	hosttest.WriteRel(t, dir, "b.txt", "hand edit\n")
	claim(t, "me", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "fix: mine")); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q, want only the claimed a.txt", got)
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); !strings.Contains(st, " M b.txt") {
		t.Fatalf("b.txt is no longer dirty: %q", st)
	}
}

func TestCommitTakesPeerClaimedPath(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "peer's\n")
	claim(t, "peer", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "fix: a", "a.txt")); err != nil {
		t.Fatalf("a peer's claim must not refuse the commit: %v", err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q, want a.txt", got)
	}
}

func TestCommitInLinkedLaneIgnoresPeerEditTimes(t *testing.T) {
	dir := commitRepo(t)
	hosttest.In(t, dir, "branch", "-m", "main")
	lane := paths.Real(filepath.Join(t.TempDir(), "lane"))
	hosttest.In(t, dir, "worktree", "add", "-q", "-b", "lane", lane)
	now := time.Now().Unix() + 20
	for _, tc := range []struct{ root, file string }{{lane, "a.txt"}, {lane, "b.txt"}, {dir, "c.txt"}} {
		hosttest.WriteRel(t, tc.root, tc.file, "edited\n")
		claimAt(t, "me", "", filepath.Join(tc.root, tc.file), now-10)
		claimAt(t, "peer", "", filepath.Join(tc.root, tc.file), now)
		if _, err := commitRun(commitReq(tc.root, "fix: "+tc.file, tc.file)); err != nil {
			t.Fatalf("%s edited last by the peer: %v", tc.file, err)
		}
	}
}

func TestCommitRename(t *testing.T) {
	dir := commitRepo(t)
	hosttest.In(t, dir, "mv", "a.txt", "renamed.txt")
	if _, err := commitRun(commitReq(dir, "refactor: rename", "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if got := commitFiles(t, dir); !strings.Contains(got, "R100\ta.txt\trenamed.txt") {
		t.Fatalf("commit holds %q, want the rename", got)
	}
}

func TestCommitDeletion(t *testing.T) {
	dir := commitRepo(t)
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := commitRun(commitReq(dir, "chore: drop b", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "D\tb.txt" {
		t.Fatalf("commit holds %q, want the deletion", got)
	}
}

func TestCommitNewUntrackedFile(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "new.txt", "x\ny\n")
	out, err := commitRun(commitReq(dir, "feat: new", "new.txt"))
	if err != nil || !strings.Contains(out, "new.txt  +2 −0") || !strings.Contains(out, "create mode 100644 new.txt") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "A\tnew.txt" {
		t.Fatalf("commit holds %q, want the new file", got)
	}
}

func TestCommitLeavesOthersStagedEntryStaged(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "mine\n")
	hosttest.WriteRel(t, dir, "c.txt", "staged by a peer\n")
	hosttest.In(t, dir, "add", "c.txt")
	out, err := commitRun(commitReq(dir, "fix: a", "a.txt"))
	if err != nil || !strings.Contains(out, "left staged: 1") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q, want only a.txt", got)
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); !strings.Contains(st, "M  c.txt") {
		t.Fatalf("c.txt is no longer staged: %q", st)
	}
}

func TestCommitStripsFootersAndTrailingBlanks(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	msg := "fix: a  \n\nwhy it matters  \nCo-Authored-By: Claude <noreply@anthropic.com>\n\n"
	out, err := commitRun(commitReq(dir, msg, "a.txt"))
	if err != nil || !strings.Contains(out, "dropped 1 AI-attribution footer") || !strings.Contains(out, "trimmed trailing") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if got := hosttest.In(t, dir, "log", "-1", "--format=%B"); strings.Contains(got, "Co-Authored") || strings.TrimSpace(got) != "fix: a\n\nwhy it matters" {
		t.Fatalf("message = %q", got)
	}
}

func TestCommitRefusesNonConventionalSubject(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	_, err := commitRun(commitReq(dir, "updated a", "a.txt"))
	wantCode(t, err, 1)
	if !strings.Contains(err.Error(), "<type>[(scope)][!]: <description>") {
		t.Fatalf("error = %v, want the expected shape named", err)
	}
}

func TestCommitLongSubjectWarnsAndCommits(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	subject := "fix: " + strings.Repeat("x", shell.SubjectWarnLen+1-len("fix: "))
	out, err := commitRun(commitReq(dir, subject, "a.txt"))
	if err != nil || !strings.Contains(out, fmt.Sprintf("warning: subject is %d characters", shell.SubjectWarnLen+1)) {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if got := strings.TrimSpace(hosttest.In(t, dir, "log", "-1", "--format=%s")); got != subject {
		t.Fatal("the long subject was not committed unchanged")
	}
}

func TestCommitDryRunChangesNothing(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	hosttest.WriteRel(t, dir, "new.txt", "y\n")
	head, before := hosttest.In(t, dir, "rev-parse", "HEAD"), hosttest.In(t, dir, "status", "--porcelain")
	req := commitReq(dir, "fix: a", "a.txt", "new.txt")
	req.DryRun = true
	out, err := commitRun(req)
	if err != nil || !strings.Contains(out, "a.txt") || !strings.Contains(out, "new.txt") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if hosttest.In(t, dir, "rev-parse", "HEAD") != head || hosttest.In(t, dir, "status", "--porcelain") != before {
		t.Fatal("--dry-run changed HEAD or the index")
	}
}

func TestCommitRefusesDirectories(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "sub/f.txt", "x\n")
	for _, arg := range []string{".", "sub", "su*"} {
		_, err := commitRun(commitReq(dir, "fix: a", arg))
		wantCode(t, err, 2)
	}
}

func TestCommitNoPathsNoClaimsIsUsageError(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	_, err := commitRun(commitReq(dir, "fix: a"))
	wantCode(t, err, 2)
	if !strings.Contains(err.Error(), "name your files") {
		t.Fatalf("error = %v", err)
	}
}

func TestCommitRenameWhoseSourceIsPeerClaimed(t *testing.T) {
	dir := commitRepo(t)
	hosttest.In(t, dir, "mv", "a.txt", "renamed.txt")
	claim(t, "peer", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "refactor: rename", "renamed.txt")); err != nil {
		t.Fatalf("a peer's claim on the rename source must not refuse: %v", err)
	}
	if got := commitFiles(t, dir); !strings.Contains(got, "renamed.txt") {
		t.Fatalf("commit holds %q, want the rename", got)
	}
}

func TestCommitNoPathsLeavesSubagentClaimsOut(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	hosttest.WriteRel(t, dir, "b.txt", "leaf's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	claimAs(t, "me", "leaf-1", filepath.Join(dir, "b.txt"))
	if _, err := commitRun(commitReq(dir, "fix: main")); err != nil {
		t.Fatalf("a subagent's claim must not stop main's bare commit: %v", err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q, want only main's a.txt", got)
	}
	if err := os.Remove(filepath.Join(ClaimsDir(), fmt.Sprintf("%d-me.jsonl", peerPid))); err != nil {
		t.Fatal(err)
	}
	claimAs(t, "me", "leaf-1", filepath.Join(dir, "b.txt")) // a subagent's claims alone leave nothing to take
	_, err := commitRun(commitReq(dir, "fix: leaf"))
	wantCode(t, err, 2)
}

func TestCommitNoPathsTakesOnlyMainClaims(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "fix: main")); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q", got)
	}
}

func TestCommitCLIDispatchBareTakesOwnSessionClaims(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "fix: x")); err != nil {
		t.Fatalf("a dispatched bare commit takes the session's claims: %v", err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q", got)
	}
}

func TestCommitCLIDispatchTakesSessionClaims(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	hosttest.WriteRel(t, dir, "b.txt", "sibling's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	claimAs(t, "me", "leaf-2", filepath.Join(dir, "b.txt"))
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := commitRun(commitReq(dir, "fix: x", f)); err != nil {
			t.Fatalf("%s: a session or sibling claim must not refuse a named file: %v", f, err)
		}
	}
}

func TestCommitCLIDispatchCommitsEveryNamedFile(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "mine\n")
	hosttest.WriteRel(t, dir, "b.txt", "another dispatch's\n")
	claimAs(t, "codex-session", "d1", filepath.Join(dir, "a.txt"))
	claimAs(t, "codex-session", "d2", filepath.Join(dir, "b.txt"))
	for _, f := range []string{"b.txt", "a.txt"} {
		if _, err := commitRun(commitReq(dir, "fix: mine", f)); err != nil {
			t.Fatalf("%s: another dispatch's claim must not refuse: %v", f, err)
		}
	}
}

func TestCommitCLIDispatchCommitsUnclaimedFile(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchDepthEnv, "1")
	hosttest.WriteRel(t, dir, "c.txt", "leaf's own\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	if _, err := commitRun(commitReq(dir, "fix: leaf", "c.txt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\tc.txt" {
		t.Fatalf("commit holds %q", got)
	}
}

func TestCommitKeepsBodySentenceMentioningAttribution(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	msg := "fix: a\n\nExplain why Co-authored-by: Claude is forbidden.\n\nFixes #4\nCo-Authored-By: Claude <noreply@anthropic.com>"
	out, err := commitRun(commitReq(dir, msg, "a.txt"))
	if err != nil || !strings.Contains(out, "dropped 1 AI-attribution") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	want := "fix: a\n\nExplain why Co-authored-by: Claude is forbidden.\n\nFixes #4"
	if got := strings.TrimSpace(hosttest.In(t, dir, "log", "-1", "--format=%B")); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestCommitWarnsOnLongMessageNeverRefuses(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	out, err := commitRun(commitReq(dir, "fix: a\n\none\ntwo\n\nBREAKING CHANGE: x\nFixes #1", "a.txt"))
	if err != nil || !strings.Contains(out, "warning: message has 3 lines") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	hosttest.WriteRel(t, dir, "a.txt", "y\n")
	out, err = commitRun(commitReq(dir, "fix: a\n\none\n\nBREAKING CHANGE: x\nFixes #1", "a.txt"))
	if err != nil || strings.Contains(out, "warning") {
		t.Fatalf("a subject, one body line and the trailers must not warn: out = %q, err = %v", out, err)
	}
}

func TestCommitSubjectLengthCountsCharactersNotBytes(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "x\n")
	subject := "fix: " + strings.Repeat("é", shell.SubjectWarnLen-len("fix: "))
	out, err := commitRun(commitReq(dir, subject, "a.txt"))
	if err != nil || strings.Contains(out, "warning") {
		t.Fatalf("a subject of exactly %d characters must not warn: out = %q, err = %v", shell.SubjectWarnLen, out, err)
	}
}

func TestCommitPartlyStagedFileCommitsWholeWorkingTreeContent(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "one\n")
	hosttest.In(t, dir, "add", "a.txt")
	hosttest.WriteRel(t, dir, "a.txt", "one\ntwo\n")
	out, err := commitRun(commitReq(dir, "fix: a", "a.txt"))
	if err != nil || !strings.Contains(out, "a.txt is partly staged; the commit takes its whole working-tree content") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if got := hosttest.In(t, dir, "show", "HEAD:a.txt"); got != "one\ntwo\n" {
		t.Fatalf("HEAD:a.txt = %q, want the whole working-tree file", got)
	}
}

// indexBytes is the index file's content, found through git so a linked worktree works too.
func indexBytes(t *testing.T, dir string) []byte {
	t.Helper()
	p := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCommitFailedCommitLeavesIndexByteIdentical(t *testing.T) {
	dir := commitRepo(t)
	hook := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-commit"))
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteRel(t, dir, "b.txt", "unrelated unstaged edit\n")
	hosttest.WriteRel(t, dir, "new.txt", "selected\n")
	before := indexBytes(t, dir)
	_, err := commitRun(commitReq(dir, "feat: new", "new.txt"))
	if err == nil {
		t.Fatal("the pre-commit hook exits 1, so the commit must fail")
	}
	if !bytes.Equal(before, indexBytes(t, dir)) {
		t.Fatal("a failed commit left the index changed")
	}
}

func TestCommitRefusesWhenIndexIsLocked(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "new.txt", "x\n")
	lock := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index")) + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := commitRun(commitReq(dir, "feat: new", "new.txt"))
	wantCode(t, err, 1)
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Fatal("the verb removed a lock it does not own")
	}
}

// failingCommitIndex stages new.txt as runCommit's add would and returns the index bytes before and after.
func failingCommitIndex(t *testing.T, dir string) (idx string, before, after []byte) {
	t.Helper()
	hosttest.WriteRel(t, dir, "new.txt", "x\n")
	idx = strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	before = indexBytes(t, dir)
	hosttest.In(t, dir, "add", "new.txt")
	return idx, before, indexBytes(t, dir)
}

func TestUndoAddRestoresUnderLockAndLeavesNoLock(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	if err := undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBytes(t, dir), before) {
		t.Fatal("the index is not byte identical to before")
	}
	if _, err := os.Stat(idx + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.lock left behind: %v", err)
	}
}

func TestUndoAddWithForeignLockUnstagesOurFilesOnly(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	shortUnstageWait(t, 50*time.Millisecond)
	if err := os.WriteFile(idx+".lock", []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if err == nil || !strings.Contains(err.Error(), "held by another process") ||
		!strings.Contains(err.Error(), "git rm --cached") {
		t.Fatalf("err = %v, want the fallback named", err)
	}
	if b, _ := os.ReadFile(idx + ".lock"); string(b) != "theirs" {
		t.Fatal("the other process's lock was touched")
	}
}

func TestUndoAddKeepsPeerStagedEntry(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	hosttest.WriteRel(t, dir, "c.txt", "peer\n")
	hosttest.In(t, dir, "add", "c.txt") // a peer's add lands between our add and the rollback
	err := undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if err == nil || !strings.Contains(err.Error(), "index changed since our add") {
		t.Fatalf("err = %v", err)
	}
	st := hosttest.In(t, dir, "status", "--porcelain")
	if !strings.Contains(st, "M  c.txt") || strings.Contains(st, "A  new.txt") {
		t.Fatalf("status = %q, want the peer's entry kept and ours unstaged", st)
	}
	if _, err := os.Stat(idx + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.lock left behind: %v", err)
	}
}

// shortUnstageWait shrinks the unstage retry bound for one test.
func shortUnstageWait(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldStep := unstageWait, unstageStep
	unstageWait, unstageStep = wait, wait/20+time.Millisecond
	t.Cleanup(func() { unstageWait, unstageStep = oldWait, oldStep })
}

func TestUndoAddRetriesUnstageUntilForeignLockReleases(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	shortUnstageWait(t, 500*time.Millisecond)
	if err := os.WriteFile(idx+".lock", []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.Remove(idx + ".lock")
	}()
	err := undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if err == nil || strings.Contains(err.Error(), "remain staged") {
		t.Fatalf("err = %v, want the unstage to succeed after the lock cleared", err)
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); strings.Contains(st, "A  new.txt") {
		t.Fatalf("status = %q, new.txt still staged", st)
	}
}

func TestUndoAddStuckLockNamesStagedFilesAndCommand(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	shortUnstageWait(t, 100*time.Millisecond)
	if err := os.WriteFile(idx+".lock", []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if err == nil || !strings.Contains(err.Error(), "remain staged: new.txt") ||
		!strings.Contains(err.Error(), "git rm --cached -f -- new.txt") || strings.Contains(err.Error(), "unstaged only") {
		t.Fatalf("err = %v, want the staged file and the unstage command, no claim of an unstage", err)
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); !strings.Contains(st, "A  new.txt") {
		t.Fatalf("status = %q, new.txt should still be staged", st)
	}
}

func TestCommitFailedCommitKeepsIndexModeAndBytes(t *testing.T) {
	hosttest.SkipOnWindows(t, "unix permission bits and sh hook")
	for _, mode := range []os.FileMode{0o600, 0o640} {
		for _, file := range []string{"new.txt", "a.txt"} {
			t.Run(fmt.Sprintf("%o/%s", mode, file), func(t *testing.T) {
				dir := commitRepo(t)
				hook := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-commit"))
				if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				hosttest.WriteRel(t, dir, file, "changed\n")
				// a newer mtime makes git's status refresh want to rewrite the index
				later := time.Now().Add(time.Minute)
				if err := os.Chtimes(filepath.Join(dir, "b.txt"), later, later); err != nil {
					t.Fatal(err)
				}
				idx := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index"))
				if err := os.Chmod(idx, mode); err != nil {
					t.Fatal(err)
				}
				bytesBefore, staged := indexBytes(t, dir), hosttest.In(t, dir, "ls-files", "-s")
				if _, err := commitRun(commitReq(dir, "feat: change", file)); err == nil {
					t.Fatal("the pre-commit hook exits 1, so the commit must fail")
				}
				if fi, err := os.Stat(idx); err != nil || fi.Mode().Perm() != mode {
					t.Fatalf("index mode = %v (%v), want %v", fi.Mode().Perm(), err, mode)
				}
				if got := hosttest.In(t, dir, "ls-files", "-s"); got != staged {
					t.Fatalf("staged set = %q, want %q", got, staged)
				}
				if !bytes.Equal(bytesBefore, indexBytes(t, dir)) {
					t.Fatal("a failed commit left the index bytes changed")
				}
			})
		}
	}
}

func TestUndoAddKeepsIndexMode(t *testing.T) {
	hosttest.SkipOnWindows(t, "unix permission bits")
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "new.txt", "x\n")
	idx := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	if err := os.Chmod(idx, 0o600); err != nil {
		t.Fatal(err)
	}
	_, before, mode, err := indexSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "new.txt") // git add rewrites the index as 0644
	if err := undoAdd(context.Background(), dir, idx, before, indexBytes(t, dir), mode, []string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(idx); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("index mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestCommitHookReformatsAddedFileLeavesItUntracked(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh hook")
	dir := commitRepo(t)
	hook := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-commit"))
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho reformatted > new.txt\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteRel(t, dir, "new.txt", "selected\n")
	old := renameIndex
	renameIndex = func(string, string) error { return errors.New("injected") } // forces the unstage fallback
	t.Cleanup(func() { renameIndex = old })
	_, err := commitRun(commitReq(dir, "feat: new", "new.txt"))
	if err == nil {
		t.Fatal("the hook exits 1, so the commit must fail")
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); !strings.Contains(st, "?? new.txt") {
		t.Fatalf("status = %q, want new.txt untracked again", st)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "reformatted\n" {
		t.Fatalf("working-tree content = %q, want the hook's rewrite kept", b)
	}
}

func TestUndoAddPanicBetweenCreateAndRenameLeavesNoLock(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	old := renameIndex
	renameIndex = func(string, string) error { panic("injected") }
	t.Cleanup(func() { renameIndex = old })
	func() {
		defer func() { _ = recover() }()
		_ = undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	}()
	if _, err := os.Stat(idx + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.lock left behind after a panic: %v", err)
	}
}

func TestUndoAddRenameFailureLeavesNoLockAndUnstages(t *testing.T) {
	dir := commitRepo(t)
	idx, before, after := failingCommitIndex(t, dir)
	old := renameIndex
	renameIndex = func(string, string) error { return errors.New("injected") }
	t.Cleanup(func() { renameIndex = old })
	_ = undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if _, err := os.Stat(idx + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index.lock left behind: %v", err)
	}
	if st := hosttest.In(t, dir, "status", "--porcelain"); strings.Contains(st, "A  new.txt") {
		t.Fatalf("status = %q, new.txt still staged", st)
	}
}

func TestUndoAddNeverUnstagesAFileStagedBeforeTheRun(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "pre.txt", "mine\n")
	hosttest.In(t, dir, "add", "pre.txt")
	idx, before, after := failingCommitIndex(t, dir)
	hosttest.WriteRel(t, dir, "c.txt", "peer\n")
	hosttest.In(t, dir, "add", "c.txt") // forces the fallback
	_ = undoAdd(context.Background(), dir, idx, before, after, 0o644, []string{"new.txt"})
	if st := hosttest.In(t, dir, "status", "--porcelain"); !strings.Contains(st, "A  pre.txt") || strings.Contains(st, "A  new.txt") {
		t.Fatalf("status = %q, want pre.txt still staged and new.txt not", st)
	}
}

// The verb ticks a named task at once, notes the owned file it left out, and refuses to name the ticked task again.
func wantPending(t *testing.T, dir, sha string, ids ...string) {
	t.Helper()
	pending, _, err := plans.PendingCommit(dir, sha, nil)
	var have []string
	for _, r := range pending {
		have = append(have, r.IDs...)
	}
	if err != nil || !slices.Equal(have, ids) {
		t.Fatalf("pending ticks for %s = %v (err %v), want %v", sha, have, err, ids)
	}
}

func TestCommitTicksPlansItself(t *testing.T) {
	dir := commitRepo(t)
	planPath := filepath.Join(plans.Home(dir), time.Now().Format(time.DateOnly)+"-p.md")
	hosttest.WriteRel(t, filepath.Dir(planPath), filepath.Base(planPath), "- [ ] 1.1 Pair owns: a.txt, b.txt\n")
	hosttest.WriteRel(t, dir, "a.txt", "changed\n")
	req := commitReq(dir, "feat: a.txt", "a.txt")
	req.Tasks = []string{"1.1"}
	out, err := commitRun(req)
	if err != nil || !strings.Contains(out, "owns: b.txt") || !strings.Contains(out, "the verifier did not start") {
		t.Fatalf("want the missing owned file and the verifier-not-started line (err=%v):\n%s", err, out)
	}
	sha := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "--short=7", "HEAD"))
	got, err := os.ReadFile(planPath)
	if err != nil || string(got) != "- [ ] 1.1 Pair owns: a.txt, b.txt\n" {
		t.Fatalf("the verifier cannot start under go test, so the plan stays open: %q err=%v", got, err)
	}
	wantPending(t, dir, sha, "1.1")
	if _, _, err := plans.TickCommit(dir, sha, nil, nil); err != nil { // what the detached verifier does when green
		t.Fatal(err)
	}
	hosttest.WriteRel(t, dir, "b.txt", "changed\n")
	req = commitReq(dir, "feat: b.txt", "b.txt")
	req.Tasks = []string{"1.1"}
	out, err = commitRun(req)
	message := hosttest.In(t, dir, "show", "-s", "--format=%B", "HEAD")
	if err != nil || !strings.Contains(out, "already ticked") || strings.Contains(message, "Task:") {
		t.Fatalf("a ticked task is a note and the commit goes on without its trailer: %v\n%s\n%s", err, out, message)
	}
}

func TestCommitTaskTrailersNoteUnknownIDs(t *testing.T) {
	dir := commitRepo(t)
	name := time.Now().Format(time.DateOnly) + "-named.md"
	plan := filepath.Join(plans.Home(dir), name)
	hosttest.WriteRel(t, filepath.Dir(plan), name, "- [ ] 1.1 First owns: fresh.txt\n- [ ] 1.2 Second owns: fresh.txt\n")
	hosttest.WriteRel(t, dir, "fresh.txt", "new\n")
	req := commitReq(dir, "feat: named", "fresh.txt")
	req.Plan, req.Tasks = plan, []string{"1.1", "1.9"}
	out, err := commitRun(req)
	if err != nil || !strings.Contains(out, "nearest ids: 1.1, 1.2") {
		t.Fatalf("an unknown task is a note, not a refusal: %s, %v", out, err)
	}
	message := hosttest.In(t, dir, "show", "-s", "--format=%B", "HEAD")
	if !strings.Contains(message, "Task: "+strings.TrimSuffix(name, ".md")+" 1.1") || strings.Contains(message, "1.9") {
		t.Fatalf("the known task keeps its trailer, the unknown one has none: %s", message)
	}
	hosttest.WriteRel(t, dir, "fresh.txt", "newer\n")
	req.Tasks = []string{"1.1", "1.2"}
	if _, err := commitRun(req); err != nil {
		t.Fatal(err)
	}
	message = hosttest.In(t, dir, "show", "-s", "--format=%B", "HEAD")
	for _, id := range req.Tasks {
		if !strings.Contains(message, "Task: "+strings.TrimSuffix(name, ".md")+" "+id) {
			t.Fatalf("missing trailer: %s", message)
		}
	}
}

func TestCommitFileIDs(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "one\n")
	hosttest.WriteRel(t, dir, "b.txt", "two\n")
	list, err := Candidates(context.Background(), dir)
	if err != nil || len(list) != 2 || list[1] != FileID("b.txt")+" b.txt" {
		t.Fatalf("Candidates = %v, %v", list, err)
	}
	hosttest.WriteRel(t, dir, "c.txt", "three\n") // a changed set keeps b.txt's id
	if _, err = commitRun(commitReq(dir, "fix: b", FileID("b.txt"))); err != nil {
		t.Fatal(err)
	}
	if got := commitFiles(t, dir); strings.TrimSpace(got) != "M\tb.txt" {
		t.Fatalf("commit holds %q, want only b.txt", got)
	}
}

func TestCommitSubjectType(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "2026-01-01-p.md")
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), "- [ ] Phase 1.1 — Do the thing (S) — owns: a.go\n")
	got, err := commitSubject(CommitRequest{Type: "feat", Tasks: []string{"Phase 1.1"}, Plan: plan})
	if err != nil || got != "feat: Do the thing (S)" {
		t.Fatalf("title subject = %q, %v", got, err)
	}
	if got, _ = commitSubject(CommitRequest{Type: "fix", Message: "bare"}); got != "fix: bare" {
		t.Fatalf("prefixed subject = %q", got)
	}
	if _, err = commitSubject(CommitRequest{Type: "nope", Message: "x"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestCommitFileIDLiteralNameWins(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "one\n")
	hosttest.WriteRel(t, dir, "@lit", "two\n")
	if _, err := commitRun(commitReq(dir, "fix: lit", "@lit")); err != nil {
		t.Fatal(err)
	}
	if got := commitFiles(t, dir); strings.TrimSpace(got) != "A\t@lit" {
		t.Fatalf("commit holds %q, want only @lit", got)
	}
}
