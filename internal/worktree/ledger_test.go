package worktree

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func ledgerFile(t *testing.T, main, name string) string {
	t.Helper()
	path := filepath.Join(memory.Dir(main), "worktrees", name+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ledger %s: %v", path, err)
	}
	return string(data)
}

func ledgerPath(main, name string) string {
	return filepath.Join(memory.Dir(main), "worktrees", name+".md")
}

func TestLedgerLifecycle(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home) // AppendLedger resolves the worktree root as ~/worktrees/<repo>
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}

	// Create opens the ledger: Status open and the base sha recorded.
	var log bytes.Buffer
	dest, err := Create(repo, "ledgered", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("create warnings: %s", log.String())
	}
	text := ledgerFile(t, main, "ledgered")
	base := revParse(t, main, "HEAD")
	for _, want := range []string{"# Worktree ledgered\n", "- Status: open\n", "- Base: " + base + " (HEAD)\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("fresh ledger missing %q:\n%s", want, text)
		}
	}

	// Append from inside the worktree lands in the log; from a non-worktree dir it is a no-op.
	ok, err := AppendLedger(dest, "update note", time.Now())
	if err != nil || !ok {
		t.Fatalf("AppendLedger(worktree) = %v, %v; want true, nil", ok, err)
	}
	if text := ledgerFile(t, main, "ledgered"); !strings.Contains(text, "update note") {
		t.Fatalf("ledger missing the appended line:\n%s", text)
	}
	ok, err = AppendLedger(main, "must not land", time.Now())
	if err != nil || ok {
		t.Fatalf("AppendLedger(main checkout) = %v, %v; want false, nil", ok, err)
	}
	if text := ledgerFile(t, main, "ledgered"); strings.Contains(text, "must not land") {
		t.Fatalf("non-worktree append reached the ledger:\n%s", text)
	}

	// A merge closes the ledger: ## Closed carries the via, merge sha and the base..head commit list.
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "work on ledger")
	wtHead := revParse(t, dest, "HEAD")
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-ledgered")
	mergeSHA := revParse(t, main, "HEAD")
	if err := CloseLedger(main, "ledgered", LedgerClose{Via: "merge", MergeSHA: mergeSHA, Head: wtHead}); err != nil {
		t.Fatal(err)
	}
	text = ledgerFile(t, main, "ledgered")
	for _, want := range []string{"- Status: closed\n", "## Closed\n", "- Via: merge\n", "- Merge: " + mergeSHA + "\n", "work on ledger"} {
		if !strings.Contains(text, want) {
			t.Fatalf("closed ledger missing %q:\n%s", want, text)
		}
	}

	// A second close (the Clean after a merge) only appends one removal line to the log.
	if err := CloseLedger(main, "ledgered", LedgerClose{Via: "clean", Head: wtHead, BranchDeleted: true}); err != nil {
		t.Fatal(err)
	}
	if text = ledgerFile(t, main, "ledgered"); !strings.Contains(text, "removed via clean, branch deleted") {
		t.Fatalf("second close did not append the removal line:\n%s", text)
	}

	// OpenLedger on a closed ledger reopens it.
	if err := OpenLedger(main, "ledgered", LedgerInfo{
		Path: dest, Branch: "worktree-ledgered", Base: base, BaseRef: "HEAD", Session: "s2",
	}); err != nil {
		t.Fatal(err)
	}
	text = ledgerFile(t, main, "ledgered")
	for _, want := range []string{"- Status: open\n", "reopened"} {
		if !strings.Contains(text, want) {
			t.Fatalf("reopened ledger missing %q:\n%s", want, text)
		}
	}
}

// TestCleanForceClosesLedger is the regression test for the CloseLedger call in cleanforce.go:
// force-removing a dirty or unmerged worktree must still record the close, via "clean --force".
func TestCleanForceClosesLedger(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	var log bytes.Buffer
	dest, err := Create(repo, "forced", CreateOptions{Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("create warnings: %s", log.String())
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "unmerged work")

	result, err := CleanForce(repo, "forced", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Removed {
		t.Fatalf("want removal, got refusal: %s", result.Reason)
	}
	text := ledgerFile(t, repo.Main, "forced")
	for _, want := range []string{"- Status: closed\n", "## Closed\n", "- Via: clean --force\n", "- Branch: deleted\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("ledger after clean --force missing %q:\n%s", want, text)
		}
	}
}

// TestAppendLedgerOutsideManagedWorktree documents the two false-no-error cases: a plain directory
// that is no repo at all, and a git repo that has no ledger for its checkout.
func TestAppendLedgerOutsideManagedWorktree(t *testing.T) {
	ok, err := AppendLedger(t.TempDir(), "x", time.Now())
	if err != nil || ok {
		t.Fatalf("AppendLedger(non-repo) = %v, %v; want false, nil", ok, err)
	}
	main := gitRepo(t) // a repo, but ResolveRepo's root never held a Create-made worktree here
	t.Setenv("HOME", t.TempDir())
	ok, err = AppendLedger(main, "x", time.Now())
	if err != nil || ok {
		t.Fatalf("AppendLedger(unmanaged repo) = %v, %v; want false, nil", ok, err)
	}
	if _, err := os.Stat(ledgerPath(main, "anything")); !os.IsNotExist(err) {
		t.Fatalf("a ledger file appeared for an unmanaged append: %v", err)
	}
}

func ledgerSetup(t *testing.T, name string) (*Repo, string, string) {
	t.Helper()
	home, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	dest, err := Create(repo, name, CreateOptions{Log: &log})
	if err != nil || log.Len() != 0 {
		t.Fatalf("create: %v %s", err, log.String())
	}
	return repo, main, dest
}

// mergeKept commits work in dest, merges it into main and closes the ledger as `merge --no-clean` does.
func mergeKept(t *testing.T, main, dest, name, msg string) {
	t.Helper()
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", msg)
	head := revParse(t, dest, "HEAD")
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-"+name)
	if err := CloseLedger(main, name, LedgerClose{Via: "merge", MergeSHA: revParse(t, main, "HEAD"), Head: head}); err != nil {
		t.Fatal(err)
	}
}

// Findings 2, 3 and 5: resume reopens with a fresh Opened section; the next close lists only new work and the final branch state.
func TestLedgerResumeAfterMergeKept(t *testing.T) {
	repo, main, dest := ledgerSetup(t, "fx")
	mergeKept(t, main, dest, "fx", "first work")
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "second work")
	if err := resumeLedger(repo, "fx", dest, "s2"); err != nil {
		t.Fatal(err)
	}
	text := ledgerFile(t, main, "fx")
	if ledgerField(text, "Status") != "open" || !strings.Contains(text, "\n## Opened\n") || !strings.Contains(text, "- Session: s2\n") {
		t.Fatalf("resume did not reopen with a new Opened section:\n%s", text)
	}
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-fx")
	var warn bytes.Buffer
	res, err := CleanWithLog(repo, "fx", false, &warn)
	if err != nil || !res.Removed || warn.Len() != 0 {
		t.Fatalf("clean: %+v %v %s", res, err, warn.String())
	}
	text = ledgerFile(t, main, "fx")
	last := text[strings.LastIndex(text, "\n## Closed\n"):]
	if !strings.Contains(last, "second work") || strings.Contains(last, "first work") {
		t.Fatalf("second close must list only commits since the reopen:\n%s", last)
	}
	if !strings.Contains(last, "- Branch: deleted\n") {
		t.Fatalf("close must record the final branch state:\n%s", last)
	}
}

func TestLedgerAppendReopensClosed(t *testing.T) {
	_, main, dest := ledgerSetup(t, "fa")
	mergeKept(t, main, dest, "fa", "first work")
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "later work")
	ok, err := AppendLedger(dest, "commit later work", time.Now())
	if err != nil || !ok {
		t.Fatalf("append = %v, %v", ok, err)
	}
	if text := ledgerFile(t, main, "fa"); ledgerField(text, "Status") != "open" || !strings.Contains(text, "commit later work") {
		t.Fatalf("append did not reopen the closed ledger:\n%s", text)
	}
}

// Finding 5: merge (branch kept) then clean must not leave "Branch: kept" in the Closed section.
func TestLedgerMergeThenCleanBranchState(t *testing.T) {
	repo, main, dest := ledgerSetup(t, "fb")
	mergeKept(t, main, dest, "fb", "work")
	if res, err := CleanWithLog(repo, "fb", false, nil); err != nil || !res.BranchDeleted {
		t.Fatalf("clean: %+v %v", res, err)
	}
	if text := ledgerFile(t, main, "fb"); strings.Contains(text, "- Branch: kept\n") || !strings.Contains(text, "- Branch: deleted\n") {
		t.Fatalf("closed ledger shows a stale branch state:\n%s", text)
	}
}

// Finding 4: a ledger failure during clean is written to the supplied writer.
func TestLedgerCleanWarningsUseWriter(t *testing.T) {
	repo, main, _ := ledgerSetup(t, "fc")
	path := ledgerPath(main, "fc")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "- Base: ", "- Basis: ", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	if _, err := CleanWithLog(repo, "fc", false, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "warning: ledger") {
		t.Fatalf("warning not routed to the writer: %q", warn.String())
	}
}

// Finding 1: ledger writers take the memory bank lock.
func TestLedgerTakesBankLock(t *testing.T) {
	_, main, dest := ledgerSetup(t, "fd")
	old := memory.BankLockWait
	memory.BankLockWait = 100 * time.Millisecond
	t.Cleanup(func() { memory.BankLockWait = old })
	var err error
	lockErr := memory.WithBankLock(time.Second, func() error {
		err = CloseLedger(main, "fd", LedgerClose{Via: "merge", Head: revParse(t, dest, "HEAD")})
		return nil
	})
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	if !errors.Is(err, memory.ErrBankBusy) {
		t.Fatalf("CloseLedger under a held bank lock = %v; want ErrBankBusy", err)
	}
}

func TestSetLedgerTaskFillsUnclaimed(t *testing.T) {
	_, main, dest := ledgerSetup(t, "wt-task")
	path := memory.Dir(main) + "/worktrees/wt-task.md"
	if ok, err := SetLedgerTask(dest, "1.2 in plan-x", time.Now()); !ok || err != nil {
		t.Fatalf("SetLedgerTask = %v, %v", ok, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- Task: 1.2 in plan-x\n") || strings.Contains(string(data), "unclaimed") {
		t.Fatalf("Task line not filled:\n%s", data)
	}
	if ok, _ := SetLedgerTask(dest, "other", time.Now()); ok {
		t.Fatal("a ledger already naming a task must stay untouched")
	}
}
