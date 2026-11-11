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

	"github.com/edotau/claude-code/internal/paths"
)

// archiveCaps are the per-file caps that trigger archiving on a durable bank file; the churny
// session-state files never do.
var archiveCaps = map[string]int{
	"conventions.md": fileCaps["conventions.md"],
	"decisionLog.md": fileCaps["decisionLog.md"],
}

// bottomAppended marks files whose newest entries land at the BOTTOM, so the settle split runs on
// the reversed list; decisionLog.md is newest-first and keeps the straight split.
var bottomAppended = map[string]bool{"conventions.md": true}

// archiveOverflow moves the oldest overflow blocks of root's durable files into the same store's
// archive/, returning how many moved. The archive stays local, so no credential gate applies.
func archiveOverflow(root string, now time.Time) (int, error) {
	dir := Dir(root)
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

// archivePath is the <dir>/archive/<base>-<yyyy-mm>.md shape shared by archiveFile and rotateSessionHistory.
func archivePath(dir, base string, now time.Time) string {
	return filepath.Join(dir, "archive", strings.TrimSuffix(base, ".md")+"-"+now.Format("2006-01")+".md")
}

// splitOverflow reads path and splits off its oldest blocks beyond capLines; an absent or in-cap file has none.
func splitOverflow(path string, capLines int) (header string, keep, overflow []string, err error) {
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		return "", nil, nil, err
	}
	header, keep, overflow = overflowSplit(filepath.Base(path), string(data), capLines)
	return header, keep, overflow, nil
}

// overflowSplit is splitOverflow on typed file base's bytes already read; an in-cap body has no overflow.
func overflowSplit(base, body string, capLines int) (header string, keep, overflow []string) {
	if logicalLineCount(body) <= capLines {
		return "", nil, nil
	}
	header, blocks := entryBlocks(base, body)
	keep, overflow = settledSplit(base, header, blocks, capLines)
	return header, keep, overflow
}

// archiveFile prunes path under the bank lock, so a concurrent append cannot land between its read and rewrite;
// the oldest overflow is archived first so nothing is lost if the rewrite fails.
func archiveFile(path string, capLines int, now time.Time) (moved int, err error) {
	err = WithBankLock(BankLockWait, func() error {
		header, keep, overflow, err := splitOverflow(path, capLines)
		if err != nil || len(overflow) == 0 {
			return err
		}
		if err := appendBlocks(archivePath(filepath.Dir(path), filepath.Base(path), now), overflow); err != nil {
			return err
		}
		if err := paths.AtomicWrite(path, []byte(rejoin(header, keep)), 0o600); err != nil {
			return err
		}
		moved = len(overflow)
		return nil
	})
	return moved, err
}

// PromoteTargets are the `memory promote --to` destinations.
var PromoteTargets = []string{"home", "rules", "skill", "claude-md"}

// promoteJudgmentTarget names where a judgment promotion lands, for the operator to act on by hand.
var promoteJudgmentTarget = map[string]string{
	"rules":     "rules/standards/<topic>.md or rules/software/<lang>/<topic>.md",
	"skill":     "skills/<name>/SKILL.md or its references/",
	"claude-md": "CLAUDE.md Hard rules (gated: 3+ entries AND a hard prohibition)",
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

// afterPromoteMove runs between promote's move and its settle; tests race a rival bank-lock taker there.
var afterPromoteMove = func() {}

// promoteBlockToHome moves one id-marked block from root's local bank into the matching home-bank
// file, preserving its id, and settles both.
func promoteBlockToHome(root, id string, dryRun bool, now time.Time) (string, error) {
	dir := Dir(root)
	if isHomeBank(root) {
		return "", errors.New("root's bank IS the home bank — nothing to promote")
	}
	var file, block, src string
	homeDir := HomeBankDir()
	moveErr := WithBankLock(BankLockWait, func() error {
		var err error
		if file, block, src, err = findBlock(dir, id); err != nil {
			return err
		}
		if dryRun {
			return nil
		}
		_, _, homeErr := extractBlock(homeDir, id)
		inHome := homeErr == nil // an existing home copy is kept, never appended twice
		snap, err := snapshotPaths(src, filepath.Join(homeDir, file))
		if err != nil {
			return err
		}
		// Any failure restores both files, so the block stays in exactly one bank (the local one).
		return restoreOnError(func() error {
			if !inHome {
				if err := appendBlocks(filepath.Join(homeDir, file), []string{block}); err != nil {
					return err
				}
			}
			if err := removeBlock(src, file, id); err != nil {
				return err
			}
			afterPromoteMove()
			return settleLocked(root, now) // same critical section: no writer slips in
		}(), snap)
	})
	if moveErr != nil {
		return "", moveErr
	}
	homePath := filepath.Join(homeDir, file)
	if dryRun {
		return fmt.Sprintf("would move [%s] %s -> %s\n\n%s\n", id, src, homePath, block), nil
	}
	return fmt.Sprintf("promoted [%s] local -> home (%s)\n", id, homePath), nil
}

// promoteJudgment finds id in root's local bank (falling back to the home bank) and prints it with
// target and the embedded judgment procedure for a human/agent to apply by hand; it is model
// residue, so Go never writes target itself. `claude-md` is gated: it only prints inside a dream
// run's own promote call (isDreamChild); elsewhere there is no cross-pollination check (3+ entries,
// hard prohibition) behind it.
func promoteJudgment(root, id, to, target string) (string, error) {
	if to == "claude-md" && !isDreamChild() {
		return "", fmt.Errorf("--to claude-md only runs inside `memory dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory promote %s --to rules`", id, id)
	}
	dir := Dir(root)
	_, block, _, err := findBlock(dir, id)
	if err != nil && !isHomeBank(root) {
		_, block, _, err = findBlock(HomeBankDir(), id)
	}
	if err != nil {
		return "", err
	}
	ref, err := Reference("promote")
	if err != nil {
		return "", err
	}
	return block + "\n\ntarget: " + target + "\n\n" + ref + "\n", nil
}

// blockIn returns the block in path (parsed under typed file name's grammar) that owns id; "" when path lacks it.
func blockIn(path, name, id string) (string, error) {
	data, err := readIfExists(path)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(data), "<!-- id:"+id+" -->") {
		return "", nil
	}
	_, blocks := entryBlocks(name, string(data))
	for _, b := range blocks {
		if ownerID(b) == id { // its own marker, not a quote of it in the body
			return b, nil
		}
	}
	return "", nil
}

// extractBlock finds the id-marked block in dir's typed files, returning its filename and text.
func extractBlock(dir, id string) (string, string, error) {
	for name := range idFiles {
		b, err := blockIn(filepath.Join(dir, name), name, id)
		if err != nil {
			return "", "", err
		}
		if b != "" {
			return name, b, nil
		}
	}
	return "", "", errNoBlock
}

// findBlock is extractBlock, then dir/archive/<stem>-*.md (dream overflow); src is the file holding the block.
func findBlock(dir, id string) (file, block, src string, err error) {
	if file, block, err = extractBlock(dir, id); err == nil {
		return file, block, filepath.Join(dir, file), nil
	}
	for name := range idFiles {
		matches, _ := filepath.Glob(filepath.Join(dir, "archive", strings.TrimSuffix(name, ".md")+"-*.md"))
		for _, p := range matches {
			if b, rerr := blockIn(p, name, id); rerr == nil && b != "" {
				return name, b, p, nil
			}
		}
	}
	return "", "", "", errNoBlock
}

// removeBlock drops the id-marked block from path (parsed under typed file name's grammar), rewriting it without it.
func removeBlock(path, name, id string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	header, blocks := entryBlocks(name, string(data))
	var kept []string
	for _, b := range blocks {
		if ownerID(b) != id {
			kept = append(kept, b)
		}
	}
	return paths.AtomicWrite(path, []byte(rejoin(header, kept)), 0o600)
}

// budgetSplit keeps blocks from the top while header+kept stays within capLines; the rest overflow.
// blocks arrives newest-first, so walking forward keeps the newest and settles the oldest end.
func budgetSplit(header string, blocks []string, capLines int) (keep, overflow []string) {
	used := logicalLineCount(header)
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
	n := logicalLineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
