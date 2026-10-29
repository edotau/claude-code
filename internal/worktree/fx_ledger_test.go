package worktree

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func fxSetup(t *testing.T, name string) (*Repo, string, string) {
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

// fxMergeKept commits work in dest, merges it into main and closes the ledger as `merge --no-clean` does.
func fxMergeKept(t *testing.T, main, dest, name, msg string) {
	t.Helper()
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", msg)
	head := revParse(t, dest, "HEAD")
	commitIn(t, main, "merge", "-q", "--no-edit", "worktree-"+name)
	if err := CloseLedger(main, name, LedgerClose{Via: "merge", MergeSHA: revParse(t, main, "HEAD"), Head: head}); err != nil {
		t.Fatal(err)
	}
}

// Findings 2, 3 and 5: resume reopens with a fresh Opened section; the next close lists only new work and the final branch state.
func TestFxLedgerResumeAfterMergeKept(t *testing.T) {
	repo, main, dest := fxSetup(t, "fx")
	fxMergeKept(t, main, dest, "fx", "first work")
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

func TestFxLedgerAppendReopensClosed(t *testing.T) {
	_, main, dest := fxSetup(t, "fa")
	fxMergeKept(t, main, dest, "fa", "first work")
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
func TestFxLedgerMergeThenCleanBranchState(t *testing.T) {
	repo, main, dest := fxSetup(t, "fb")
	fxMergeKept(t, main, dest, "fb", "work")
	if res, err := CleanWithLog(repo, "fb", false, nil); err != nil || !res.BranchDeleted {
		t.Fatalf("clean: %+v %v", res, err)
	}
	if text := ledgerFile(t, main, "fb"); strings.Contains(text, "- Branch: kept\n") || !strings.Contains(text, "- Branch: deleted\n") {
		t.Fatalf("closed ledger shows a stale branch state:\n%s", text)
	}
}

// Finding 4: a ledger failure during clean is written to the supplied writer.
func TestFxLedgerCleanWarningsUseWriter(t *testing.T) {
	repo, main, _ := fxSetup(t, "fc")
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
func TestFxLedgerTakesBankLock(t *testing.T) {
	_, main, dest := fxSetup(t, "fd")
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

func TestFxSetLedgerTaskFillsUnclaimed(t *testing.T) {
	_, main, dest := fxSetup(t, "wt-task")
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
