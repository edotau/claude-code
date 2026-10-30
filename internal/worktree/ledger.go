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

// LedgerInfo describes the checkout and starting commit recorded when a ledger opens.
type LedgerInfo struct {
	Path, Branch, Base, BaseRef, Session string
	Now                                  time.Time
}

// LedgerClose records the outcome of a merge or removal without requiring the checkout to still exist.
type LedgerClose struct {
	Via, MergeSHA, Head string
	BranchDeleted       bool
	Now                 time.Time
}

// OpenLedger creates a ledger, leaves an open ledger unchanged, or reopens a closed ledger.
func OpenLedger(main, name string, info LedgerInfo) error {
	if err := ledgerName(name); err != nil {
		return err
	}
	dir, err := memory.EnsureRepoDir(main)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "worktrees", name+".md")
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
	repo, err := ResolveRepo(cwd)
	if err != nil {
		return false, nil
	}
	out, err := gitCmd(context.Background(), "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return false, err
	}
	top, err := canonical(strings.TrimSpace(string(out)))
	if err != nil {
		return false, err
	}
	if !paths.SamePath(filepath.Dir(top), repo.Root) {
		return false, nil
	}
	name := filepath.Base(top)
	if _, err := ResolveWorktree(repo, name); err != nil {
		return false, err
	}
	path := filepath.Join(memory.Dir(repo.Main), "worktrees", name+".md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	appended := false
	err = withLedgerLockWithin(wait, path, func() error {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if ledgerField(string(data), "Status") == "closed" {
			info, err := reopenInfo(repo, name, top, "")
			if err != nil {
				return err
			}
			info.Now = now
			data = []byte(reopenedText(string(data), info))
		}
		next := edit(string(data))
		if next == string(data) {
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

// CloseLedger records a first close in full and subsequent removals in the log; missing ledgers are ignored.
func CloseLedger(main, name string, c LedgerClose) error {
	if err := ledgerName(name); err != nil {
		return err
	}
	path := filepath.Join(memory.Dir(main), "worktrees", name+".md")
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
		text := string(data)
		branch := "kept"
		if c.BranchDeleted {
			branch = "deleted"
		}
		if ledgerField(text, "Status") == "closed" {
			text = ledgerLog(text, fmt.Sprintf("removed via %s, branch %s", c.Via, branch), c.Now)
			if i := strings.LastIndex(text, "\n## Closed\n"); i >= 0 && c.BranchDeleted {
				text = text[:i] + strings.Replace(text[i:], "- Branch: kept\n", "- Branch: deleted\n", 1)
			}
			return paths.AtomicWrite(path, []byte(text), 0o600)
		}
		base := strings.Fields(ledgerField(openSection(text), "Base"))
		if len(base) == 0 || c.Head == "" {
			return fmt.Errorf("ledger %s: base and closing HEAD are required", name)
		}
		out, err := gitCmd(context.Background(), "-C", main, "log", "--format=%h %s", base[0]+".."+c.Head, "--").Output()
		if err != nil {
			return fmt.Errorf("ledger %s: list commits: %w", name, err)
		}
		merge := c.MergeSHA
		if merge == "" {
			merge = "none"
		}
		text = strings.Replace(text, "- Status: open\n", "- Status: closed\n", 1)
		text = ledgerLog(text, "closed via "+c.Via, c.Now)
		text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\n## Closed\n\n- Via: %s\n- Merge: %s\n- Closed: %s\n- Branch: %s\n- Commits:\n",
			ledgerLine(c.Via), ledgerLine(merge), ledgerTime(c.Now), branch)
		commits := strings.TrimSpace(string(out))
		if commits == "" {
			text += "  - none\n"
		} else {
			for _, commit := range strings.Split(commits, "\n") {
				text += "  - " + commit + "\n"
			}
		}
		return paths.AtomicWrite(path, []byte(text), 0o600)
	})
}

// withLedgerLock runs fn under the memory bank lock (so an update or dream cannot interleave) and the file lock.
func withLedgerLock(path string, fn func() error) error {
	return withLedgerLockWithin(memory.BankLockWait, path, fn)
}

// withLedgerLockWithin is withLedgerLock with its own bank-lock wait, for best-effort callers inside a hook budget.
func withLedgerLockWithin(wait time.Duration, path string, fn func() error) error {
	return memory.WithWritableBankLock(wait, func() error { return paths.WithFileLock(path, fn) })
}

// openSection is the text of the latest "## Opened" section, or the whole ledger when it was never reopened.
func openSection(text string) string {
	i := strings.LastIndex(text, "\n## Opened\n")
	if i < 0 {
		return text
	}
	return text[i:]
}

// reopenText rewrites a closed ledger as open with a fresh Opened section; the caller holds the lock.
func reopenedText(text string, info LedgerInfo) string {
	text = strings.Replace(text, "- Status: closed\n", "- Status: open\n", 1)
	text = ledgerLog(text, "reopened", info.Now)
	text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\n## Opened\n\n- Branch: %s\n- Base: %s (%s)\n- Opened: %s\n- Session: %s\n",
		ledgerLine(info.Branch), ledgerLine(info.Base), ledgerLine(info.BaseRef), ledgerTime(info.Now), ledgerLine(info.Session))
	return text
}

// reopenInfo describes work resuming in a kept worktree: base is the merge-base with the main checkout's HEAD.
func reopenInfo(repo *Repo, name, dest, session string) (LedgerInfo, error) {
	head, err := headOfContext(context.Background(), dest)
	if err != nil {
		return LedgerInfo{}, err
	}
	base, err := gitCmd(context.Background(), "-C", repo.Main, "merge-base", "HEAD", head).Output()
	if err != nil {
		return LedgerInfo{}, fmt.Errorf("resolve resume base: %w", err)
	}
	branch, err := currentWorktreeBranch(dest)
	if err != nil {
		return LedgerInfo{}, err
	}
	return LedgerInfo{Path: dest, Branch: branch, Base: strings.TrimSpace(string(base)), BaseRef: "merge-base", Session: session}, nil
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
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
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
