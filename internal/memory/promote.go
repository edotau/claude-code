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

// archiveCaps are the per-file caps that trigger archiving on a durable file; session-state files never do.
var archiveCaps = map[string]int{
	"conventions.md": fileCaps["conventions.md"],
	"decisionLog.md": fileCaps["decisionLog.md"],
}

// bottomAppended marks files whose newest entries are at the BOTTOM, so the split runs on the reversed list.
var bottomAppended = map[string]bool{"conventions.md": true}

// archiveOverflow moves the oldest overflow blocks of root's durable files to its archive/ and returns the count.
func archiveOverflow(root string, now time.Time) (int, error) {
	b := BankFor(root)
	total := 0
	for name, capLines := range archiveCaps {
		n, err := archiveFile(b, name, capLines, now)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// settledSplit splits blocks into (keep, overflow) at capLines, oldest into overflow; bottom-appended files reverse.
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

// archivePath is b's archive/<name>-<yyyy-mm>.md destination of archiveFile.
func archivePath(b Bank, name string, now time.Time) string {
	return b.Sub("archive", strings.TrimSuffix(name, ".md")+"-"+now.Format("2006-01")+".md")
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
	header, keep, overflow = overflowSplit(logicalName(path), string(data), capLines)
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

// archiveFile prunes b's typed file name under the bank lock, archiving the oldest overflow first so nothing is lost.
func archiveFile(b Bank, name string, capLines int, now time.Time) (moved int, err error) {
	path := b.Path(name)
	err = withFlatLock(BankLockWait, func() error {
		header, keep, overflow, err := splitOverflow(path, capLines)
		if err != nil || len(overflow) == 0 {
			return err
		}
		archive := archivePath(b, name, now)
		existing, err := readFile(archive)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_, held := entryBlocks(name, string(existing))
		heldSet := make(map[string]bool, len(held))
		for _, b := range held {
			heldSet[strings.TrimSpace(b)] = true
		}
		// a retry after a failed rewrite must not archive the same blocks twice
		fresh := slices.DeleteFunc(slices.Clone(overflow), func(o string) bool { return heldSet[strings.TrimSpace(o)] })
		if len(fresh) > 0 {
			if err := appendBlocks(archive, fresh); err != nil {
				return err
			}
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

// Promote moves block id to home (settled both sides) or prints it with its target for a human to apply by hand.
func Promote(root, id, to string, dryRun bool, now time.Time) (string, error) {
	_, b, err := ResolveBank(root)
	if err != nil {
		return "", err
	}
	if target, ok := promoteJudgmentTarget[to]; ok {
		return promoteJudgment(b, id, to, target)
	}
	if to != "home" {
		return "", fmt.Errorf("--to %q: want %s", to, strings.Join(PromoteTargets, ", "))
	}
	return promoteBlockToHome(b, id, dryRun, now)
}

// afterPromoteMove runs between promote's move and its settle; tests race a rival bank-lock taker there.
var afterPromoteMove = func() {}

// promoteBlockToHome moves one id-marked block from root's local bank to the home file, keeping its id.
func promoteBlockToHome(b Bank, id string, dryRun bool, now time.Time) (string, error) {
	home := HomeBank()
	if b.Home() {
		return "", errors.New("root's bank IS the home bank — nothing to promote")
	}
	if dryRun {
		return promoteDryRun(b, id)
	}
	var file string
	moveErr := withFlatLock(BankLockWait, func() (err error) { file, err = promoteLocked(b, home, id, now); return err })
	if moveErr != nil {
		return "", moveErr
	}
	return fmt.Sprintf("promoted [%s] local -> home (%s)\n", id, home.Path(file)), nil
}

// promoteLocked does the move under the bank lock and returns the block's file name.
func promoteLocked(b, home Bank, id string, now time.Time) (file string, err error) {
	var block, src string
	if file, block, src, err = findBlock(b, id); err != nil {
		return file, err
	}
	_, _, homeErr := extractBlock(home, id)
	if homeErr != nil && !errors.Is(homeErr, errNoBlock) {
		return file, homeErr // an unreadable home file is not "absent": appending could duplicate the block
	}
	inHome := homeErr == nil // an existing home copy is kept, never appended twice
	snap, err := snapshotPaths(src, home.Path(file))
	if err != nil {
		return file, err
	}
	// Any failure restores both files, so the block stays in exactly one bank (the local one).
	return file, restoreOnError(func() error {
		if !inHome {
			if err := appendBlocks(home.Path(file), []string{block}); err != nil {
				return err
			}
		}
		if err := removeBlock(src, file, id); err != nil {
			return err
		}
		afterPromoteMove()
		return settleLockedBank(b, now)
	}(), snap)
}

// promoteDryRun previews the move read-only: no bank lock, no layout stamp.
func promoteDryRun(b Bank, id string) (string, error) {
	if err := checkFlat(); err != nil {
		return "", err
	}
	file, block, src, err := findBlock(b, id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("would move [%s] %s -> %s\n\n%s\n", id, src, HomeBank().Path(file), block), nil
}

// promoteJudgment prints id with its target and procedure; `claude-md` only prints inside a dream run (isDreamChild).
func promoteJudgment(own Bank, id, to, target string) (string, error) {
	if to == "claude-md" && !isDreamChild() {
		return "", fmt.Errorf("--to claude-md only runs inside `memory dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory promote %s --to rules`", id, id)
	}
	_, block, _, err := findBlock(own, id)
	if errors.Is(err, errNoBlock) && !own.Home() { // a real read error is not "absent"
		_, block, _, err = findBlock(HomeBank(), id)
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

// extractBlock finds the id-marked block in b's typed files, returning its logical filename and text.
func extractBlock(b Bank, id string) (string, string, error) {
	for name := range idFiles {
		blk, err := blockIn(b.Path(name), name, id)
		if err != nil {
			return "", "", err
		}
		if blk != "" {
			return name, blk, nil
		}
	}
	return "", "", errNoBlock
}

// findBlock is extractBlock, then b's archive/<stem>-*.md (dream overflow); src is the file holding the block.
func findBlock(b Bank, id string) (file, block, src string, err error) {
	if file, block, err = extractBlock(b, id); err == nil {
		return file, block, b.Path(file), nil
	} else if !errors.Is(err, errNoBlock) {
		return "", "", "", err
	}
	for name := range idFiles {
		ap := b.Sub("archive", strings.TrimSuffix(name, ".md")+"-*.md")
		for _, p := range globIn(filepath.Dir(ap), filepath.Base(ap)) {
			blk, rerr := blockIn(p, name, id)
			if rerr != nil {
				return "", "", "", rerr
			}
			if blk != "" {
				return name, blk, p, nil
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

// budgetSplit keeps newest-first blocks while header+kept fits capLines; the rest overflow.
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

// blockCost is a block's line cost including the blank separator rejoin inserts (none before block 0 sans header).
func blockCost(block string, i, used int) int {
	n := logicalLineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
