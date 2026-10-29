package worktree

import (
	"bytes"
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
	ok, err := AppendLedger(dest, "harvest note", time.Now())
	if err != nil || !ok {
		t.Fatalf("AppendLedger(worktree) = %v, %v; want true, nil", ok, err)
	}
	if text := ledgerFile(t, main, "ledgered"); !strings.Contains(text, "harvest note") {
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
