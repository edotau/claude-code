package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// archiveCaps are the per-file caps that trigger archiving on a durable file; session-state files never do.
var archiveCaps = map[string]int{
	"conventions.md": fileCaps["conventions.md"],
	"decisionLog.md": fileCaps["decisionLog.md"],
}

// bottomAppended marks files whose newest entries are at the BOTTOM, so the split runs on the reversed list.
var bottomAppended = map[string]bool{"conventions.md": true}

// archiveOverflow moves the oldest overflow blocks of root's durable files to its archive/ and returns the count.
func archiveOverflow(now time.Time) (int, error) {
	total := 0
	for name, capLines := range archiveCaps {
		n, err := archiveFile(name, capLines, now)
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

// archivePath is the archive/<name>-<yyyy-mm>.md destination of archiveFile.
func archivePath(name string, now time.Time) string {
	return Sub("archive", strings.TrimSuffix(name, ".md")+"-"+now.Format("2006-01")+".md")
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

// archiveFile prunes the typed file name under the bank lock, archiving the oldest overflow first so nothing is lost.
func archiveFile(name string, capLines int, now time.Time) (moved int, err error) {
	path := Path(name)
	err = withFlatLock(BankLockWait, func() error {
		header, keep, overflow, err := splitOverflow(path, capLines)
		if err != nil || len(overflow) == 0 {
			return err
		}
		archive := archivePath(name, now)
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
var PromoteTargets = []string{"rules", "skill", "claude-md"}

// promoteJudgmentTarget names where a judgment promotion lands, for the operator to act on by hand.
var promoteJudgmentTarget = map[string]string{
	"rules":     "rules/standards/<topic>.md or rules/software/<lang>/<topic>.md",
	"skill":     "skills/<name>/SKILL.md or its references/",
	"claude-md": "CLAUDE.md Hard rules (gated: 3+ entries AND a hard prohibition)",
}

// Promote moves block id to home (settled both sides) or prints it with its target for a human to apply by hand.
func Promote(id, to string) (string, error) {
	if target, ok := promoteJudgmentTarget[to]; ok {
		return promoteJudgment(id, to, target)
	}
	return "", fmt.Errorf("--to %q: want %s", to, strings.Join(PromoteTargets, ", "))
}

// promoteJudgment prints id with its target and procedure; `claude-md` only prints inside a dream run (isDreamChild).
func promoteJudgment(id, to, target string) (string, error) {
	if to == "claude-md" && !isDreamChild() {
		return "", fmt.Errorf("--to claude-md only runs inside `memory dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory promote %s --to rules`", id, id)
	}
	_, block, _, err := findBlock(id)
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

// extractBlock finds the id-marked block in the typed files, returning its logical filename and text.
func extractBlock(id string) (string, string, error) {
	for name := range idFiles {
		blk, err := blockIn(Path(name), name, id)
		if err != nil {
			return "", "", err
		}
		if blk != "" {
			return name, blk, nil
		}
	}
	return "", "", errNoBlock
}

// findBlock is extractBlock, then the archive/<stem>-*.md (dream overflow); src is the file holding the block.
func findBlock(id string) (file, block, src string, err error) {
	if file, block, err = extractBlock(id); err == nil {
		return file, block, Path(file), nil
	} else if !errors.Is(err, errNoBlock) {
		return "", "", "", err
	}
	for name := range idFiles {
		ap := Sub("archive", strings.TrimSuffix(name, ".md")+"-*.md")
		for _, p := range transcript.GlobIn(filepath.Dir(ap), filepath.Base(ap)) {
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
