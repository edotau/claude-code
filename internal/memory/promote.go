package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textsim"
)

// promoteCaps are the per-file caps that trigger promote/archive on the LOCAL bank — only durable
// files promote; the churny session-state files never do.
var promoteCaps = map[string]int{
	"conventions.md": FileCaps["conventions.md"],
	"decisionLog.md": FileCaps["decisionLog.md"],
}

// promoteJaccard is the block token-overlap above which a local block already exists in the home bank.
const promoteJaccard = 0.6

// minEntryTokens filters trivially-short blocks (headers, one-word bullets) out of dedup matching.
const minEntryTokens = 4

// bottomAppended marks files whose newest entries land at the BOTTOM, so the settle split runs on
// the reversed list; decisionLog.md is newest-first and keeps the straight split.
var bottomAppended = map[string]bool{"conventions.md": true}

// homeBankLockPath serializes all home-bank writers (promote, archive) behind one lock file under
// state — never beside the committed home bank.
func homeBankLockPath() string { return filepath.Join(storeLockDir(HomeBankDir()), ".promote.lock") }

// PromoteLocalToGlobal moves settled overflow from root's local durable files into the shared home
// bank (deduped) and prunes local back under cap. No-op when local IS the home bank.
func PromoteLocalToGlobal(root string, now time.Time) (int, error) {
	if IsHomeBank(root) {
		return 0, nil
	}
	localDir, homeDir := Dir(root), HomeBankDir()
	promoted := 0
	ran, err := paths.TryFileLock(homeBankLockPath(), 2*time.Second, func() error {
		cleanupLegacyLocks(homeDir) // one-time: old dotfile locks left beside the home bank
		for name, capLines := range promoteCaps {
			n, err := promoteFile(filepath.Join(localDir, name), filepath.Join(homeDir, name), capLines, now)
			if err != nil {
				return err
			}
			promoted += n
		}
		return nil
	})
	if !ran && err == nil {
		return 0, errors.New("home bank busy (another worker holds the lock) — promotion deferred")
	}
	return promoted, err
}

// ArchiveSettledOverflow is the relief valve when local IS the home bank: no second tier to promote
// into, so overflow moves to <bank>/archive/<file>-<yyyy-mm>.md instead. No-op for an ordinary project bank.
func ArchiveSettledOverflow(root string, now time.Time) (int, error) {
	if !IsHomeBank(root) {
		return 0, nil
	}
	dir := Dir(root)
	archived := 0
	ran, err := paths.TryFileLock(homeBankLockPath(), 2*time.Second, func() error {
		cleanupLegacyLocks(dir) // one-time: old dotfile locks left beside the home bank
		for name, capLines := range promoteCaps {
			n, err := archiveFile(filepath.Join(dir, name), dir, capLines, now)
			if err != nil {
				return err
			}
			archived += n
		}
		return nil
	})
	if !ran && err == nil {
		return 0, errors.New("home bank busy (another worker holds the lock) — archive deferred")
	}
	return archived, err
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

// archivePath is the <dir>/archive/<base>-<yyyy-mm>.md shape shared by archiveFile, promoteFile
// (via archiveFile) and RotateSessionHistory.
func archivePath(dir, base string, now time.Time) string {
	return filepath.Join(dir, "archive", strings.TrimSuffix(base, ".md")+"-"+now.Format("2006-01")+".md")
}

// archiveFile prunes path to capLines by moving its SETTLED (oldest) blocks into an archive file.
func archiveFile(path, dir string, capLines int, now time.Time) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil // absent file — nothing to archive
	}
	if LogicalLineCount(string(data)) <= capLines {
		return 0, nil
	}
	base := filepath.Base(path)
	header, blocks := entryBlocks(base, string(data))

	keep, overflow := settledSplit(base, header, blocks, capLines)
	if len(overflow) == 0 {
		return 0, nil
	}

	archive := archivePath(dir, base, now)
	if err := appendBlocks(archive, overflow); err != nil {
		return 0, err // archive failed — leave the source intact (never truncate unarchived content)
	}
	if err := paths.AtomicWrite(path, []byte(rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(overflow), nil
}

// promoteFile prunes localPath to capLines by promoting the OLDEST overflow blocks into homePath.
// Near-dups of home-bank blocks simply drop (they already live there); blocks too short to dedup
// reliably are ARCHIVED beside the local bank instead of promoted — overflow is never lost, only
// re-homed.
func promoteFile(localPath, homePath string, capLines int, now time.Time) (int, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return 0, nil // absent local file — nothing to promote
	}
	if LogicalLineCount(string(data)) <= capLines {
		return 0, nil
	}
	base := filepath.Base(localPath)
	header, blocks := entryBlocks(base, string(data))

	keep, overflow := settledSplit(base, header, blocks, capLines)
	if len(overflow) == 0 {
		return 0, nil
	}

	// Credential gate: refuse the whole promote rather than spread a leaked value into the shared
	// home bank (whence it rides every session's injection). Scrub the block to unblock.
	if slices.ContainsFunc(overflow, credentialPattern.MatchString) {
		return 0, fmt.Errorf("%s: possible credential in overflow block — scrub it, then re-run promote", base)
	}

	// Dedup overflow against what's already in the home-bank file.
	hData, _ := os.ReadFile(homePath)
	_, hBlocks := entryBlocks(filepath.Base(homePath), string(hData))
	hSets := make([]map[string]bool, 0, len(hBlocks))
	for _, b := range hBlocks {
		hSets = append(hSets, blockTokens(b))
	}
	var toAppend, dropped []string
	for _, b := range overflow {
		set := blockTokens(b)
		if isNearDup(set, hSets) {
			continue // already in the home bank → safe to drop from local
		}
		if len(set) < minEntryTokens {
			dropped = append(dropped, b) // too short to dedup reliably — archive, never lose
			continue
		}
		toAppend = append(toAppend, b)
		hSets = append(hSets, set) // guard against dups WITHIN the overflow too
	}

	if len(dropped) > 0 {
		archive := archivePath(filepath.Dir(localPath), base, now)
		if err := appendBlocks(archive, dropped); err != nil {
			return 0, err // archive failed — leave the source intact (never truncate unarchived content)
		}
	}
	if len(toAppend) > 0 {
		if err := appendBlocks(homePath, toAppend); err != nil {
			return 0, err
		}
	}
	// Rewrite local with header + kept blocks (every overflow block is promoted, a home-bank dup, or archived).
	if err := paths.AtomicWrite(localPath, []byte(rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(toAppend), nil
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
	dir := Dir(root)
	if IsHomeBank(root) {
		return "", errors.New("root's bank IS the home bank — nothing to promote")
	}
	file, block, err := extractBlock(dir, id)
	if err != nil {
		return "", err
	}
	homePath := filepath.Join(HomeBankDir(), file)
	if dryRun {
		return fmt.Sprintf("would move [%s] %s -> %s\n\n%s\n", id, filepath.Join(dir, file), homePath, block), nil
	}
	if err := appendBlocks(homePath, []string{block}); err != nil {
		return "", err
	}
	if err := removeBlock(dir, file, id); err != nil {
		return "", err
	}
	if _, err := SettleBank(root, now); err != nil {
		return "", err
	}
	return fmt.Sprintf("promoted [%s] local -> home (%s)\n", id, homePath), nil
}

// promoteJudgment finds id in root's local bank (falling back to the home bank) and prints it with
// target and the embedded judgment procedure for a human/agent to apply by hand; it is model
// residue, so Go never writes target itself. `claude-md` is gated: it only prints outside a dream
// run's own promote call, where there is no cross-pollination check (3+ entries, hard prohibition)
// behind it.
func promoteJudgment(root, id, to, target string) (string, error) {
	if to == "claude-md" && !IsDreamChild() {
		return "", fmt.Errorf("--to claude-md only runs inside `memory dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory promote %s --to rules`", id, id)
	}
	dir := Dir(root)
	_, block, err := extractBlock(dir, id)
	if err != nil && !IsHomeBank(root) {
		_, block, err = extractBlock(HomeBankDir(), id)
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

// extractBlock finds the id-marked block in dir's typed files, returning its filename and text.
func extractBlock(dir, id string) (string, string, error) {
	marker := "<!-- id:" + id + " -->"
	for name := range idFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(data), marker) {
			continue
		}
		_, blocks := entryBlocks(name, string(data))
		for _, b := range blocks {
			if strings.Contains(b, marker) {
				return name, b, nil
			}
		}
	}
	return "", "", ErrNoBlock
}

// removeBlock drops the id-marked block from dir/name, rewriting the file without it.
func removeBlock(dir, name, id string) error {
	marker := "<!-- id:" + id + " -->"
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	header, blocks := entryBlocks(name, string(data))
	var kept []string
	for _, b := range blocks {
		if !strings.Contains(b, marker) {
			kept = append(kept, b)
		}
	}
	return paths.AtomicWrite(path, []byte(rejoin(header, kept)), 0o600)
}

// blockTokens is a block's lowercase token set — the signal for near-dup matching.
func blockTokens(s string) map[string]bool {
	set := map[string]bool{}
	for _, t := range textsim.Tokenize(s) {
		set[t] = true
	}
	return set
}

// isNearDup reports whether set near-duplicates any block in sets (Jaccard over promoteJaccard).
func isNearDup(set map[string]bool, sets []map[string]bool) bool {
	for _, s := range sets {
		if jaccard(set, s) > promoteJaccard {
			return true
		}
	}
	return false
}

// jaccard is |a∩b| / |a∪b|, 1 for two empty sets (vacuously identical).
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter, union := 0, len(a)
	for t := range b {
		if a[t] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// budgetSplit keeps blocks from the top while header+kept stays within capLines; the rest overflow.
// blocks arrives newest-first, so walking forward keeps the newest and settles the oldest end.
func budgetSplit(header string, blocks []string, capLines int) (keep, overflow []string) {
	used := LogicalLineCount(header)
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
	n := LogicalLineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
