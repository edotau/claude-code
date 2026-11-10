package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// restoredIndex fills the injected core up to NativeCoreCap; restored pointers beyond it go to SEARCHABLE.md, recall-only.
func restoredIndex(b *nativeBatch, ix archiveIndexes, lines []string) string {
	room := max(NativeCoreCap-len(indexLinks(ix.healed)), 0)
	var overflow []string
	if len(lines) > room {
		lines, overflow = lines[:room], lines[room:]
	}
	if len(overflow) > 0 {
		b.put(NativeSearchableFile, []byte(indexWithLines(string(ix.searchable), overflow)), ix.searchable)
	}
	return indexWithLines(ix.healed, lines)
}

// restoreNative reverses archiveNative: the topic file moves back and its recorded pointer line is re-appended
// verbatim — to MEMORY.md while the injected core has room, else to SEARCHABLE.md.
// The archived copy is classified in place, so a typed bank file or index name can never be restored
// over a live one; a live file with different bytes aborts the whole pass before anything moves.
func restoreNative(dir string, names []string, apply bool) (nativeResult, error) {
	res := nativeResult{Applied: apply}
	archiveDir := nativeArchiveDir(dir)
	todo, skipped, err := resolveTopics(archiveDir, names, "not in the archive")
	res.Skipped = skipped
	if err != nil {
		return res, err
	}
	if len(todo) == 0 || !apply {
		res.Restored = todo
		return res, nil
	}
	err = WithBankLock(BankLockWait, func() error { return restoreNativeLocked(dir, todo, &res) })
	return res, err
}

// restoreNativeLocked is restoreNative's apply path for a caller that already holds the bank lock: one batch that
// lands the files, re-points them, removes the archive copies, then strikes the record. An equal live copy (a
// crashed restore) is kept and only the archive copy goes; the reconcile repair rides in the same batch.
func restoreNativeLocked(dir string, todo []string, res *nativeResult) error {
	ix, err := healedIndex(dir)
	if err != nil {
		return err
	}
	recRel, rec := filepath.Join(nativeArchiveRel, NativeArchiveIndexFile), ix.record
	b := newNativeBatch(dir)
	moved, archived, err := stageRestoreFiles(b, dir, todo)
	if err != nil {
		return err
	}
	final := restoredIndex(b, ix, linesFor(indexLinks(string(rec)), moved))
	if ix.index != nil || final != "" { // an absent MEMORY.md stays absent when nothing is restored into it
		b.put(NativeIndexFile, []byte(final), ix.index)
	}
	for _, name := range todo {
		b.del(filepath.Join(nativeArchiveRel, name), archived[name])
	}
	if rec != nil {
		if err := b.putWithout(recRel, string(rec), todo, rec); err != nil {
			return err
		}
	}
	sha, err := b.commit("restore " + strings.Join(todo, ", "))
	if err != nil {
		return err
	}
	res.Restored, res.Commit = todo, sha
	return landedIndexOverLimit("restored", todo, final)
}

// stageRestoreFiles stages each archived file's landing in b unless an equal live copy exists; it returns the files
// it landed and every archived file's bytes.
func stageRestoreFiles(b *nativeBatch, dir string, todo []string) (moved []string, archived map[string][]byte, err error) {
	archived = map[string][]byte{}
	for _, name := range todo {
		data, err := os.ReadFile(filepath.Join(dir, nativeArchiveRel, name))
		if err != nil {
			return nil, nil, err
		}
		live, err := liveCollision(filepath.Join(dir, name), data)
		if err != nil {
			return nil, nil, fmt.Errorf("restore %s: %w", name, err)
		}
		if !live {
			b.put(name, data, nil)
			moved = append(moved, name)
		}
		archived[name] = data
	}
	return moved, archived, nil
}

// liveCollision reports whether dst is live already; a live file is accepted only when its bytes equal archived.
func liveCollision(dst string, archived []byte) (bool, error) {
	fi, err := os.Lstat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	name := filepath.Base(dst)
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s: live path is not a regular file — resolve by hand", name)
	}
	cur, err := os.ReadFile(dst)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(cur, archived) {
		return false, fmt.Errorf("%s: live file differs from the archived copy — resolve by hand", name)
	}
	return true, nil
}

// splitNativeIndex trims MEMORY.md to at most keepLines pointer lines, moving the oldest unpinned ones
// (file order = oldest first: Claude Code appends new memories at the bottom) to SEARCHABLE.md. A line
// carrying a `<!-- keep -->` marker never moves; pins claim the budget first, recency fills the rest.
// Returns the number of lines moved. apply is the caller's job — this always writes when there is work.
func splitNativeIndex(dir string, keepLines int) (int, error) {
	var moved int
	err := WithBankLock(BankLockWait, func() (err error) {
		moved, err = splitNativeIndexLocked(dir, keepLines)
		return err
	})
	return moved, err
}

// splitNativeIndexLocked is splitNativeIndex for a caller that already holds the bank lock. Lines inside
// the generated bank region are neither pointers nor candidates and are rewritten byte-for-byte; a malformed
// region refuses the split. Pointers move even when native prose alone busts the loader limit — that is reported.
func splitNativeIndexLocked(dir string, keepLines int) (int, error) {
	indexPath := filepath.Join(dir, NativeIndexFile)
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if err := checkBankRegion(string(raw)); err != nil {
		return 0, fmt.Errorf("%s: %w", NativeIndexFile, err)
	}
	lines := strings.Split(string(raw), "\n")
	pinnedCount, unpinnedIdx := pointerLines(lines, raw)
	budget := max(keepLines-pinnedCount, 0)
	if len(unpinnedIdx) <= budget {
		return 0, checkNativeIndexLimits(string(raw))
	}
	drop := unpinnedIdx[:len(unpinnedIdx)-budget] // oldest unpinned pointer lines, file order preserved
	kept, overflow := partitionLines(lines, drop)
	// One batch, SEARCHABLE.md first: a crash between the two leaves a duplicated pointer, never a lost one.
	b := newNativeBatch(dir)
	searchable, err := readIfExists(filepath.Join(dir, NativeSearchableFile))
	if err != nil {
		return 0, err
	}
	b.put(NativeSearchableFile, []byte(indexWithLines(string(searchable), overflow)), searchable)
	trimmed := strings.Join(kept, "\n")
	b.put(NativeIndexFile, []byte(trimmed), raw)
	if _, err := b.commit(fmt.Sprintf("split index: %d pointer(s) to %s", len(overflow), NativeSearchableFile)); err != nil {
		return 0, err
	}
	if err := checkNativeIndexLimits(trimmed); err != nil {
		return len(overflow), fmt.Errorf("moved %d pointer line(s), but the native prose that stays still busts the limit — %w", len(overflow), err)
	}
	return len(overflow), nil
}

// pointerLines counts the pinned (<!-- keep -->) pointer lines and lists the unpinned ones' indexes, skipping the generated region.
func pointerLines(lines []string, raw []byte) (pinnedCount int, unpinnedIdx []int) {
	region, _ := bankRegionLines(string(raw)) // same scan checkBankRegion just passed
	for i, line := range lines {
		if _, ok := linkTarget(line); !ok || region[i] {
			continue
		}
		if strings.Contains(line, "<!-- keep -->") {
			pinnedCount++
		} else {
			unpinnedIdx = append(unpinnedIdx, i)
		}
	}
	return pinnedCount, unpinnedIdx
}

// partitionLines splits lines into those kept and the dropped ones named by index, order preserved.
func partitionLines(lines []string, drop []int) (kept, overflow []string) {
	dropSet := map[int]bool{}
	for _, i := range drop {
		dropSet[i] = true
		overflow = append(overflow, lines[i])
	}
	for i, line := range lines {
		if !dropSet[i] {
			kept = append(kept, line)
		}
	}
	return kept, overflow
}
