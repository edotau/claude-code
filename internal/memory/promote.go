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

// promoteJaccard is the block token-overlap above which a local block already exists in global.
const promoteJaccard = 0.6

// minEntryTokens filters trivially-short blocks (headers, one-word bullets) out of dedup matching.
const minEntryTokens = 4

// bottomAppended marks files whose newest entries land at the BOTTOM, so the settle split runs on
// the reversed list; decisionLog.md is newest-first and keeps the straight split.
var bottomAppended = map[string]bool{"conventions.md": true}

// globalLockPath serializes all global-bank writers (promote, archive) behind one lock file.
func globalLockPath() string {
	dir := GlobalDir()
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, ".promote.lock")
}

// PromoteLocalToGlobal moves settled overflow from root's local durable files into the shared global
// bank (deduped) and prunes local back under cap. No-op when local IS global.
func PromoteLocalToGlobal(root string, now time.Time) (int, error) {
	if IsGlobal(root) {
		return 0, nil
	}
	localDir, globalDir := Dir(root), GlobalDir()
	promoted := 0
	ran, err := paths.TryFileLock(globalLockPath(), 2*time.Second, func() error {
		for name, capLines := range promoteCaps {
			n, err := promoteFile(filepath.Join(localDir, name), filepath.Join(globalDir, name), capLines, now)
			if err != nil {
				return err
			}
			promoted += n
		}
		return nil
	})
	if !ran && err == nil {
		return 0, errors.New("global bank busy (another worker holds the lock) — promotion deferred")
	}
	return promoted, err
}

// ArchiveSettledOverflow is the relief valve when local IS global: no second tier to promote into, so
// overflow moves to <bank>/archive/<file>-<yyyy-mm>.md instead. No-op for an ordinary project bank.
func ArchiveSettledOverflow(root string, now time.Time) (int, error) {
	if !IsGlobal(root) {
		return 0, nil
	}
	dir := Dir(root)
	archived := 0
	ran, err := paths.TryFileLock(globalLockPath(), 2*time.Second, func() error {
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
		return 0, errors.New("global bank busy (another worker holds the lock) — archive deferred")
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
		slices.Reverse(overflow) // oldest-first, so archive/global appends stay chronological
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
	if lineCount(string(data)) <= capLines {
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

// promoteFile prunes localPath to capLines by promoting the OLDEST overflow blocks into globalPath.
// Near-dups of global blocks simply drop (they already live there); blocks too short to dedup
// reliably are ARCHIVED beside the local bank instead of promoted — overflow is never lost, only
// re-homed.
func promoteFile(localPath, globalPath string, capLines int, now time.Time) (int, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return 0, nil // absent local file — nothing to promote
	}
	if lineCount(string(data)) <= capLines {
		return 0, nil
	}
	base := filepath.Base(localPath)
	header, blocks := entryBlocks(base, string(data))

	keep, overflow := settledSplit(base, header, blocks, capLines)
	if len(overflow) == 0 {
		return 0, nil
	}

	// Credential gate: refuse the whole promote rather than spread a leaked value into the shared
	// global bank (whence it rides every session's injection). Scrub the block to unblock.
	if slices.ContainsFunc(overflow, credentialPattern.MatchString) {
		return 0, fmt.Errorf("%s: possible credential in overflow block — scrub it, then re-run promote", base)
	}

	// Dedup overflow against what's already in the global file.
	gData, _ := os.ReadFile(globalPath)
	_, gBlocks := entryBlocks(filepath.Base(globalPath), string(gData))
	gSets := make([]map[string]bool, 0, len(gBlocks))
	for _, b := range gBlocks {
		gSets = append(gSets, blockTokens(b))
	}
	var toAppend, dropped []string
	for _, b := range overflow {
		set := blockTokens(b)
		if isNearDup(set, gSets) {
			continue // already in global → safe to drop from local
		}
		if len(set) < minEntryTokens {
			dropped = append(dropped, b) // too short to dedup reliably — archive, never lose
			continue
		}
		toAppend = append(toAppend, b)
		gSets = append(gSets, set) // guard against dups WITHIN the overflow too
	}

	if len(dropped) > 0 {
		archive := archivePath(filepath.Dir(localPath), base, now)
		if err := appendBlocks(archive, dropped); err != nil {
			return 0, err // archive failed — leave the source intact (never truncate unarchived content)
		}
	}
	if len(toAppend) > 0 {
		if err := appendBlocks(globalPath, toAppend); err != nil {
			return 0, err
		}
	}
	// Rewrite local with header + kept blocks (every overflow block is promoted, a global dup, or archived).
	if err := paths.AtomicWrite(localPath, []byte(rejoin(header, keep)), 0o600); err != nil {
		return 0, err
	}
	return len(toAppend), nil
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
	used := lineCount(header)
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
	n := lineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
