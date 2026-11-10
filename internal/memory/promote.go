package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// archiveCaps are the per-file caps that trigger archiving on a durable bank file; the churny
// session-state files never do.
var archiveCaps = map[string]int{
	"conventions.md": FileCaps["conventions.md"],
	"decisionLog.md": FileCaps["decisionLog.md"],
}

// bottomAppended marks files whose newest entries land at the BOTTOM, so the settle split runs on
// the reversed list; decisionLog.md is newest-first and keeps the straight split.
var bottomAppended = map[string]bool{"conventions.md": true}

// ArchiveOverflow moves the oldest overflow blocks of root's durable files into the same store's
// archive/, returning how many moved. The archive stays local, so no credential gate applies.
func ArchiveOverflow(root string, now time.Time) (int, error) {
	dir := memstore.Dir(root)
	total := 0
	for name, capLines := range archiveCaps {
		n, err := archiveFile(filepath.Join(dir, name), capLines, now)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// settledSplit splits blocks into (keep, overflow) at capLines, oldest-first into overflow.
// Bottom-appended files split on the reversed list then restore file order, since their newest
// entries are at the BOTTOM.
func settledSplit(base, header string, blocks []string, capLines int) (keep, overflow []string) {
	if bottomAppended[base] {
		rev := slices.Clone(blocks)
		slices.Reverse(rev)
		keep, overflow = budgetSplit(header, rev, capLines)
		slices.Reverse(keep)
		slices.Reverse(overflow) // oldest-first, so archive/home appends stay chronological
		return keep, overflow
	}
	return budgetSplit(header, blocks, capLines)
}

// archivePath is the <dir>/archive/<base>-<yyyy-mm>.md shape shared by archiveFile and RotateSessionHistory.
func archivePath(dir, base string, now time.Time) string {
	return filepath.Join(dir, "archive", strings.TrimSuffix(base, ".md")+"-"+now.Format("2006-01")+".md")
}

// splitOverflow reads path and splits off its oldest blocks beyond capLines; an absent or in-cap file has none.
func splitOverflow(path string, capLines int) (header string, keep, overflow []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		return "", nil, nil, err
	}
	if bank.LogicalLineCount(string(data)) <= capLines {
		return "", nil, nil, nil
	}
	base := filepath.Base(path)
	header, blocks := bank.EntryBlocks(base, string(data))
	keep, overflow = settledSplit(base, header, blocks, capLines)
	return header, keep, overflow, nil
}

// archiveFile prunes path under the bank lock, so a concurrent append cannot land between its read and rewrite.
func archiveFile(path string, capLines int, now time.Time) (int, error) {
	var moved int
	err := memstore.WithBankLock(memstore.BankLockWait, func() (err error) {
		moved, err = archiveFileLocked(path, capLines, now)
		return err
	})
	return moved, err
}

// archiveFileLocked is archiveFile for a caller holding the bank lock; it appends the oldest overflow to the archive
// first so nothing is lost if the rewrite fails.
func archiveFileLocked(path string, capLines int, now time.Time) (int, error) {
	header, keep, overflow, err := splitOverflow(path, capLines)
	if err != nil || len(overflow) == 0 {
		return 0, err
	}
	if err := bank.AppendBlocks(archivePath(filepath.Dir(path), filepath.Base(path), now), overflow); err != nil {
		return 0, err
	}
	if err := paths.AtomicWrite(path, []byte(bank.Rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(overflow), nil
}

// PromoteTargets are the `memory promote --to` destinations.
var PromoteTargets = []string{"home", "rules", "skill", "claude-md"}

// promoteJudgmentTarget names where a judgment promotion lands, for the operator to act on by hand.
var promoteJudgmentTarget = map[string]string{
	"rules":     "rules/standards/<topic>.md or rules/software/<lang>/<topic>.md",
	"skill":     "skills/<name>/SKILL.md or its references/",
	"claude-md": "CLAUDE.md Known Gotchas (gated: 3+ entries AND a hard prohibition)",
}

// Promote moves or surfaces block id by --to: "home" is a deterministic move (local -> home bank,
// settled both sides); "rules"/"skill"/"claude-md" print the block and target for a human/agent to
// apply by hand (model residue), and never edit those targets themselves.
func Promote(root, id, to string, dryRun bool, now time.Time) (string, error) {
	if target, ok := promoteJudgmentTarget[to]; ok {
		return promoteJudgment(root, id, to, target)
	}
	if to != "home" {
		return "", fmt.Errorf("--to %q: want %s", to, strings.Join(PromoteTargets, ", "))
	}
	return promoteBlockToHome(root, id, dryRun, now)
}

// promoteBlockToHome moves one id-marked block from root's local bank into the matching home-bank
// file, preserving its id, and settles both.
func promoteBlockToHome(root, id string, dryRun bool, now time.Time) (string, error) {
	dir := memstore.Dir(root)
	if memstore.IsHomeBank(root) {
		return "", errors.New("root's bank IS the home bank — nothing to promote")
	}
	var file, block string
	homeDir := memstore.HomeBankDir()
	moveErr := memstore.WithBankLock(memstore.BankLockWait, func() error {
		var err error
		if file, block, err = extractBlock(dir, id); err != nil {
			return err
		}
		if dryRun {
			return nil
		}
		_, _, homeErr := extractBlock(homeDir, id)
		inHome := homeErr == nil // an existing home copy is kept, never appended twice or rolled back
		if !inHome {
			if err := bank.AppendBlocks(filepath.Join(homeDir, file), []string{block}); err != nil {
				return err
			}
		}
		if err := removeBlock(dir, file, id); err != nil {
			if inHome {
				return err
			}
			// roll back the home copy so the block stays in exactly one bank (the local one)
			return errors.Join(err, removeBlock(homeDir, file, id))
		}
		return nil
	})
	if moveErr != nil {
		return "", moveErr
	}
	homePath := filepath.Join(homeDir, file)
	if dryRun {
		return fmt.Sprintf("would move [%s] %s -> %s\n\n%s\n", id, filepath.Join(dir, file), homePath, block), nil
	}
	if _, err := bank.SettleBank(root, now); err != nil {
		return "", err
	}
	return fmt.Sprintf("promoted [%s] local -> home (%s)\n", id, homePath), nil
}

// promoteJudgment finds id in root's local bank (falling back to the home bank) and prints it with
// target and the embedded judgment procedure for a human/agent to apply by hand; it is model
// residue, so Go never writes target itself. `claude-md` is gated: it only prints inside a dream
// run's own promote call (IsDreamChild); elsewhere there is no cross-pollination check (3+ entries,
// hard prohibition) behind it.
func promoteJudgment(root, id, to, target string) (string, error) {
	if to == "claude-md" && !IsDreamChild() {
		return "", fmt.Errorf("--to claude-md only runs inside `memory dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory promote %s --to rules`", id, id)
	}
	dir := memstore.Dir(root)
	_, block, err := extractBlock(dir, id)
	if err != nil && !memstore.IsHomeBank(root) {
		_, block, err = extractBlock(memstore.HomeBankDir(), id)
	}
	if err != nil {
		return "", err
	}
	ref, err := bank.Reference("promote")
	if err != nil {
		return "", err
	}
	return block + "\n\ntarget: " + target + "\n\n" + ref + "\n", nil
}

// extractBlock finds the id-marked block in dir's typed files, returning its filename and text.
func extractBlock(dir, id string) (string, string, error) {
	marker := "<!-- id:" + id + " -->"
	for name := range bank.IdFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		if !strings.Contains(string(data), marker) {
			continue
		}
		_, blocks := bank.EntryBlocks(name, string(data))
		for _, b := range blocks {
			if ownsMarker(b, id) {
				return name, b, nil
			}
		}
	}
	return "", "", bank.ErrNoBlock
}

// ownsMarker reports whether id is b's own (oldest unfenced) marker, not a quote of it in the body.
func ownsMarker(b, id string) bool { return bank.OwnerID(b) == id }

// removeBlock drops the id-marked block from dir/name, rewriting the file without it.
func removeBlock(dir, name, id string) error {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	header, blocks := bank.EntryBlocks(name, string(data))
	var kept []string
	for _, b := range blocks {
		if !ownsMarker(b, id) {
			kept = append(kept, b)
		}
	}
	return paths.AtomicWrite(path, []byte(bank.Rejoin(header, kept)), 0o600)
}

// budgetSplit keeps blocks from the top while header+kept stays within capLines; the rest overflow.
// blocks arrives newest-first, so walking forward keeps the newest and settles the oldest end.
func budgetSplit(header string, blocks []string, capLines int) (keep, overflow []string) {
	used := bank.LogicalLineCount(header)
	full := false
	for i, b := range blocks {
		cost := blockCost(b, i, used)
		// The i > 0 exemption guarantees a file is never emptied — block 0 always survives.
		if full || (i > 0 && used+cost > capLines) {
			full = true // one block too big must not let a smaller LATER (older) block sneak back in
			overflow = append(overflow, b)
			continue
		}
		keep = append(keep, b)
		used += cost
	}
	return keep, overflow
}

// blockCost is a block's line cost including the blank separator rejoin inserts — absent only before
// block 0 of a header-less file.
func blockCost(block string, i, used int) int {
	n := bank.LogicalLineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
