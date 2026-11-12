package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// ledgerInfo describes the checkout and starting commit recorded when a ledger opens.
type ledgerInfo struct {
	Path, Branch, Base, BaseRef, Session string
	Now                                  time.Time
}

// LedgerClose records the outcome of a merge or removal without requiring the checkout to still exist.
type LedgerClose struct {
	Via, MergeSHA, Head string
	BranchDeleted       bool
	Now                 time.Time
}

// ledgerPath is worktree name's ledger file under the repo's memory dir.
func ledgerPath(dir, name string) string { return filepath.Join(dir, "worktrees", name+".md") }

// openLedger creates a ledger, leaves an open ledger unchanged, or reopens a closed ledger.
func openLedger(main, name string, info ledgerInfo) error {
	if err := ledgerName(name); err != nil {
		return err
	}
	dir, err := memory.EnsureRepoDir(main)
	if err != nil {
		return err
	}
	path := ledgerPath(dir, name)
	return withLedgerLock(path, func() error {
		data, err := os.ReadFile(path)
		if err == nil {
			text := string(data)
			if ledgerField(text, "Status") == "open" {
				return nil
			}
			return paths.AtomicWrite(path, []byte(reopenedText(text, info)), 0o600)
		}
		if !os.IsNotExist(err) {
			return err
		}
		text := fmt.Sprintf("# Worktree %s\n\n- Repo: %s\n- Path: %s\n- Branch: %s\n- Base: %s (%s)\n- Opened: %s\n- Session: %s\n- Task: unclaimed\n- Status: open\n\n## Log\n",
			name, ledgerLine(main), ledgerLine(info.Path), ledgerLine(info.Branch), ledgerLine(info.Base),
			ledgerLine(info.BaseRef), ledgerTime(info.Now), ledgerLine(info.Session))
		return paths.AtomicWrite(path, []byte(ledgerLog(text, "opened", info.Now)), 0o600)
	})
}

// bestEffortLedgerWait bounds the bank-lock wait of hook-side ledger edits, which run inside a hook budget.
const bestEffortLedgerWait = 750 * time.Millisecond

// ledgerLocateTimeout bounds the hook-path git rev-parse in locateLedger so a stalled git cannot outlive the hook.
const ledgerLocateTimeout = 2 * time.Second

// ledgerGitTimeout bounds the git calls made while callers hold the bank lock.
const ledgerGitTimeout = 10 * time.Second

// AppendLedger appends a timestamped entry only for a managed linked worktree with an existing ledger.
func AppendLedger(cwd, text string, now time.Time) (bool, error) {
	return editLedger(cwd, now, bestEffortLedgerWait, func(data string) string { return ledgerLog(data, text, now) })
}

// SetLedgerTask fills a managed worktree ledger's "Task: unclaimed" line; a ledger already naming a task is untouched.
func SetLedgerTask(cwd, task string, now time.Time) (bool, error) {
	task = ledgerLine(task)
	if task == "" {
		return false, nil
	}
	return editLedger(cwd, now, bestEffortLedgerWait, func(data string) string {
		if !strings.Contains(data, "\n- Task: unclaimed\n") {
			return data
		}
		return ledgerLog(strings.Replace(data, "\n- Task: unclaimed\n", "\n- Task: "+task+"\n", 1), "task claimed: "+task, now)
	})
}

// editLedger applies edit to the cwd worktree's ledger under the lock, reopening a closed one first.
func editLedger(cwd string, now time.Time, wait time.Duration, edit func(string) string) (bool, error) {
	repo, top, path, found, err := locateLedger(cwd)
	if !found || err != nil {
		return false, err
	}
	appended := false
	err = withLedgerLockWithin(wait, path, func() error {
		cur, next, err := editedLedger(repo, top, path, now, wait, edit)
		if err != nil {
			return err
		}
		if next == cur {
			return nil
		}
		if err := paths.AtomicWrite(path, []byte(next), 0o600); err != nil {
			return err
		}
		appended = true
		return nil
	})
	return appended, err
}

// locateLedger resolves the managed linked worktree holding cwd and its existing ledger; found is false for none.
func locateLedger(cwd string) (repo *Repo, top, path string, found bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), ledgerLocateTimeout)
	defer cancel()
	out, err := gitCmd(ctx, "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel").Output()
	commonDir, topLine, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if commonDir == "" {
		if code, isExit := asExitErr(err); err == nil || isExit && code == gitFatalExit && ctx.Err() == nil {
			return nil, "", "", false, nil // not inside a git repository
		}
		return nil, "", "", false, err
	}
	repo, rerr := repoFromCommonDir(cwd, commonDir)
	if rerr != nil {
		return nil, "", "", false, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	top = paths.Canon(strings.TrimSpace(topLine))
	if !paths.SamePath(filepath.Dir(top), repo.Root) {
		return nil, "", "", false, nil
	}
	name := filepath.Base(top)
	path = ledgerPath(memory.Dir(repo.Main), name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "", "", false, nil
	} else if err != nil {
		return nil, "", "", false, err
	}
	if _, err := resolveWorktree(ctx, repo, name); err != nil {
		return nil, "", "", false, err
	}
	return repo, top, path, true, nil
}

// editedLedger returns the ledger text at path (reopened if closed) and with edit applied; the caller holds the lock.
func editedLedger(repo *Repo, top, path string, now time.Time, gitWait time.Duration, edit func(string) string) (cur, next string, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	cur = string(raw)
	if ledgerField(cur, "Status") == "closed" {
		info, err := reopenInfo(repo, top, "", gitWait)
		if err != nil {
			return "", "", err
		}
		info.Now = now
		cur = reopenedText(cur, info)
	}
	return cur, edit(cur), nil
}

// CloseLedger records a first close in full and subsequent removals in the log; missing ledgers are ignored.
func CloseLedger(main, name string, c LedgerClose) error {
	if err := ledgerName(name); err != nil {
		return err
	}
	path := ledgerPath(memory.Dir(main), name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return withLedgerLock(path, func() error {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		text, err := closedLedgerText(main, name, string(data), c)
		if err != nil {
			return err
		}
		return paths.AtomicWrite(path, []byte(text), 0o600)
	})
}

// closedLedgerText is text after a close: a removal log line when already closed, else the full Closed section.
func closedLedgerText(main, name, text string, c LedgerClose) (string, error) {
	branch := "kept"
	if c.BranchDeleted {
		branch = "deleted"
	}
	if ledgerField(text, "Status") == "closed" {
		text = ledgerLog(text, fmt.Sprintf("removed via %s, branch %s", c.Via, branch), c.Now)
		if i := strings.LastIndex(text, "\n## Closed\n"); i >= 0 && c.BranchDeleted {
			text = text[:i] + strings.Replace(text[i:], "- Branch: kept\n", "- Branch: deleted\n", 1)
		}
		return text, nil
	}
	base := strings.Fields(ledgerField(openSection(text), "Base"))
	if len(base) == 0 || c.Head == "" {
		return "", fmt.Errorf("ledger %s: base and closing HEAD are required", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ledgerGitTimeout)
	defer cancel()
	out, err := gitCmd(ctx, "-C", main, "log", "--format=%h %s", base[0]+".."+c.Head, "--").Output()
	if err != nil {
		return "", fmt.Errorf("ledger %s: list commits: %w", name, err)
	}
	merge := c.MergeSHA
	if merge == "" {
		merge = "none"
	}
	text = strings.Replace(text, "- Status: open\n", "- Status: closed\n", 1)
	text = ledgerLog(text, "closed via "+c.Via, c.Now)
	text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\n## Closed\n\n- Via: %s\n- Merge: %s\n- Closed: %s\n- Branch: %s\n- Commits:\n",
		ledgerLine(c.Via), ledgerLine(merge), ledgerTime(c.Now), branch)
	return text + commitLines(strings.TrimSpace(string(out))), nil
}

// commitLines renders the closing commit list as ledger bullets, "none" when empty.
func commitLines(commits string) string {
	if commits == "" {
		return "  - none\n"
	}
	return "  - " + strings.ReplaceAll(commits, "\n", "\n  - ") + "\n"
}

// withLedgerLock runs fn under the memory bank lock (so an update or dream cannot interleave) and the file lock.
func withLedgerLock(path string, fn func() error) error {
	return withLedgerLockWithin(memory.BankLockWait, path, fn)
}

// withLedgerLockWithin is withLedgerLock with its own bank-lock wait, for best-effort callers inside a hook budget.
func withLedgerLockWithin(wait time.Duration, path string, fn func() error) error {
	return memory.WithBankLock(wait, func() error { return paths.WithFileLock(path, fn) })
}

// openSection is the text of the latest "## Opened" section, or the whole ledger when it was never reopened.
func openSection(text string) string {
	i := strings.LastIndex(text, "\n## Opened\n")
	if i < 0 {
		return text
	}
	return text[i:]
}

// reopenedText rewrites a closed ledger as open with a fresh Opened section; the caller holds the lock.
func reopenedText(text string, info ledgerInfo) string {
	text = strings.Replace(text, "- Status: closed\n", "- Status: open\n", 1)
	text = ledgerLog(text, "reopened", info.Now)
	text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\n## Opened\n\n- Branch: %s\n- Base: %s (%s)\n- Opened: %s\n- Session: %s\n",
		ledgerLine(info.Branch), ledgerLine(info.Base), ledgerLine(info.BaseRef), ledgerTime(info.Now), ledgerLine(info.Session))
	return text
}

// reopenInfo describes work resuming in a kept worktree (git bounded by gitWait): base is the merge-base with main's HEAD.
func reopenInfo(repo *Repo, dest, session string, gitWait time.Duration) (ledgerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitWait)
	defer cancel()
	head, err := headOfContext(ctx, dest)
	if err != nil {
		return ledgerInfo{}, err
	}
	base, err := gitCmd(ctx, "-C", repo.Main, "merge-base", "HEAD", head).Output()
	if err != nil {
		return ledgerInfo{}, fmt.Errorf("resolve resume base: %w", err)
	}
	branch, err := currentWorktreeBranch(dest)
	if err != nil {
		return ledgerInfo{}, err
	}
	return ledgerInfo{Path: dest, Branch: branch, Base: strings.TrimSpace(string(base)), BaseRef: "merge-base", Session: session}, nil
}

// ledgerSession is the session that opened (or last reopened) worktreePath's ledger; "" on any error or none.
func ledgerSession(worktreePath string) string {
	_, _, path, found, err := locateLedger(worktreePath)
	if err != nil || !found {
		return ""
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return ledgerField(openSection(string(text)), "Session")
}

func ledgerName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\n") {
		return fmt.Errorf("worktree ledger name %q is not usable", name)
	}
	return nil
}

func ledgerField(text, field string) string {
	prefix := "- " + field + ": "
	for line := range strings.SplitSeq(text, "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return after
		}
	}
	return ""
}

func ledgerLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func ledgerTime(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return now.Format(time.RFC3339)
}

func ledgerLog(text, entry string, now time.Time) string {
	line := "- " + ledgerTime(now) + " " + ledgerLine(entry) + "\n"
	if i := strings.Index(text, "\n## Closed\n"); i >= 0 {
		return strings.TrimRight(text[:i], "\n") + "\n" + line + text[i:]
	}
	return strings.TrimRight(text, "\n") + "\n" + line
}
