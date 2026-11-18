package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
)

// commitRepo is a repo with a.txt, b.txt and c.txt committed, a hermetic config dir, and the caller's session "me".
func commitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(proc.DispatchDepthEnv, "")
	dir := paths.Real(hosttest.Repo(t))
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		hosttest.WriteRel(t, dir, f, "base\n")
	}
	hosttest.In(t, dir, "add", ".")
	hosttest.In(t, dir, "commit", "-q", "-m", "chore: base")
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
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"schema":1,"path":%q,"agent_id":%q,"ts":%d}`+"\n", path, agent, time.Now().Unix())
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

func TestCommitRefusesPeerClaimedPath(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "peer's\n")
	claim(t, "peer", filepath.Join(dir, "a.txt"))
	head := hosttest.In(t, dir, "rev-parse", "HEAD")
	_, err := commitRun(commitReq(dir, "fix: a", "a.txt"))
	wantCode(t, err, 1)
	if !strings.Contains(err.Error(), fmt.Sprintf("a.txt is claimed by session peer (pid %d)", peerPid)) {
		t.Fatalf("error = %v, want the holder named", err)
	}
	if hosttest.In(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("a refused commit moved HEAD")
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
	if err != nil || !strings.Contains(out, "new.txt  +2 −0") {
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

func TestCommitRefusesRenameWhoseSourceIsPeerClaimed(t *testing.T) {
	dir := commitRepo(t)
	hosttest.In(t, dir, "mv", "a.txt", "renamed.txt")
	claim(t, "peer", filepath.Join(dir, "a.txt"))
	head := hosttest.In(t, dir, "rev-parse", "HEAD")
	_, err := commitRun(commitReq(dir, "refactor: rename", "renamed.txt"))
	wantCode(t, err, 1)
	if !strings.Contains(err.Error(), "a.txt is claimed by session peer") || hosttest.In(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("err = %v; the peer's source path must be named and HEAD unmoved", err)
	}
}

func TestCommitNoPathsRefusesClaimsOfSeveralAgents(t *testing.T) {
	dir := commitRepo(t)
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	hosttest.WriteRel(t, dir, "b.txt", "leaf's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	claimAs(t, "me", "leaf-1", filepath.Join(dir, "b.txt"))
	_, err := commitRun(commitReq(dir, "fix: both"))
	wantCode(t, err, 2)
	if !strings.Contains(err.Error(), "name your files") {
		t.Fatalf("error = %v", err)
	}
	if err := os.Remove(filepath.Join(ClaimsDir(), fmt.Sprintf("%d-me.jsonl", peerPid))); err != nil {
		t.Fatal(err)
	}
	claimAs(t, "me", "leaf-1", filepath.Join(dir, "a.txt")) // a subagent's claims alone are refused too
	_, err = commitRun(commitReq(dir, "fix: leaf"))
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

func TestCommitCLIDispatchNeedsNamedFiles(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	head := hosttest.In(t, dir, "rev-parse", "HEAD")
	_, err := commitRun(commitReq(dir, "fix: x"))
	wantCode(t, err, 2)
	if !strings.Contains(err.Error(), "name your files") || hosttest.In(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("err = %v, HEAD must not move", err)
	}
}

func TestCommitCLIDispatchRefusesSessionClaims(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "main's\n")
	hosttest.WriteRel(t, dir, "b.txt", "sibling's\n")
	claimAs(t, "me", "", filepath.Join(dir, "a.txt"))
	claimAs(t, "me", "leaf-2", filepath.Join(dir, "b.txt"))
	for f, who := range map[string]string{"a.txt": "this session's main agent", "b.txt": "sibling leaf leaf-2"} {
		_, err := commitRun(commitReq(dir, "fix: x", f))
		wantCode(t, err, 1)
		if !strings.Contains(err.Error(), f+" is claimed by "+who) {
			t.Fatalf("%s: error = %v, want the holder named", f, err)
		}
	}
}

func TestCommitCLIDispatchCommitsFileItClaimedItself(t *testing.T) {
	dir := commitRepo(t)
	t.Setenv(proc.DispatchIDEnv, "d1")
	hosttest.WriteRel(t, dir, "a.txt", "mine\n")
	hosttest.WriteRel(t, dir, "b.txt", "another dispatch's\n")
	claimAs(t, "codex-session", "d1", filepath.Join(dir, "a.txt"))
	claimAs(t, "codex-session", "d2", filepath.Join(dir, "b.txt"))
	if _, err := commitRun(commitReq(dir, "fix: mine", "b.txt")); err == nil || !strings.Contains(err.Error(), "b.txt is claimed by") {
		t.Fatalf("another dispatch's claim: err = %v, want a refusal naming b.txt", err)
	}
	if _, err := commitRun(commitReq(dir, "fix: mine", "a.txt")); err != nil {
		t.Fatalf("own dispatch's claim refused: %v", err)
	}
	if got := strings.TrimSpace(commitFiles(t, dir)); got != "M\ta.txt" {
		t.Fatalf("commit holds %q", got)
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
	shortUnstageWait(t, 3*time.Second)
	if err := os.WriteFile(idx+".lock", []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
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
