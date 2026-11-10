package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// topicFiles lists dir's KindTopic files; an absent dir is empty, any other read failure is returned.
func topicFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if err := checkReadable(dir, e); err != nil {
			return nil, err
		}
		if classifyMemoryFile(dir, e.Name()) == KindTopic {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// checkReadable fails on a regular .md entry that cannot be opened: ClassifyMemoryFile reads such a file as
// KindOther, which would drop it from the scan without an error.
func checkReadable(dir string, e os.DirEntry) error {
	if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, e.Name()))
	if err != nil {
		return err
	}
	return f.Close()
}

// topicName normalizes a user-supplied name to "<slug>.md" and classifies it against dir: a reserved name
// is refused outright, an absent file passes (callers skip it as not there), and anything present that is
// not an archivable topic is refused, since callers pass the result straight to a move.
func topicName(dir, arg string) (string, error) {
	name := strings.TrimSpace(arg)
	if err := validBareName(name); err != nil {
		return "", err
	}
	k, reserved := reservedNames[name]
	if !reserved && !strings.HasSuffix(name, ".md") {
		name += ".md"
		k, reserved = reservedNames[name]
	}
	if reserved {
		return "", fmt.Errorf("%q: %s, not an archivable topic", arg, k)
	}
	absent, err := absentPath(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	if absent {
		return name, nil
	}
	if k := classifyMemoryFile(dir, name); !k.Archivable() {
		return "", fmt.Errorf("%q: %s, not an archivable topic", arg, k)
	}
	return name, nil
}

// nativeArchiveDir is dir's own cold tier for archived topics and their pointer records (plan §3.2: dir/archive/native).
func nativeArchiveDir(dir string) string {
	return filepath.Join(dir, nativeArchiveRel)
}

// nativeStoreStats reports dir's injected cost plus two integrity faults across MEMORY.md + SEARCHABLE.md: a
// topic no pointer names (orphan), and a pointer whose target is gone (dangling — an answer that cannot open).
type nativeStoreStats struct {
	TopicFiles   int   // flat topic .md files (the store's own index files excluded)
	IndexEntries int   // `- [Title](topic.md)` lines in MEMORY.md — the part paid on EVERY turn
	IndexBytes   int64 // MEMORY.md's size
	TopicBytes   int64 // topic files' total size — paid only when one is recalled
	Orphans      []string
	Dangling     []string
}

// nativeResult reports one archive/restore pass; Skipped carries the reason per rejected name.
type nativeResult struct {
	Archived []string
	Restored []string
	Skipped  []string
	Applied  bool
	Commit   string // memory-Git commit the applied pass settled into; empty for a dry run or a dir outside the store
}

// nativeBatch plans dir-relative checked changes, then lands them in one pass: via the store transaction (journal,
// settle, commit) when dir is inside the store, else applied directly with the same preimage rules (dir outside the store, no repo).
type nativeBatch struct {
	base, prefix string // base is the store (prefix = dir's path inside it) or dir itself for a dir outside the store
	inStore      bool
	changes      []Change
}

func newNativeBatch(dir string) *nativeBatch {
	if rel, ok := insideStore(dir); ok {
		return &nativeBatch{base: StoreDir(), prefix: filepath.FromSlash(rel), inStore: true}
	}
	return &nativeBatch{base: dir}
}

// put plans writing data at rel on condition of preimage (nil = must not exist); equal bytes plan nothing.
func (b *nativeBatch) put(rel string, data, preimage []byte) {
	if !sameBytes(data, preimage) {
		b.changes = append(b.changes, Change{Rel: filepath.Join(b.prefix, rel), Data: data, Preimage: preimage, Checked: true})
	}
}

// sameBytes is bytes.Equal with nil (absent) kept apart from an empty file.
func sameBytes(a, b []byte) bool { return (a == nil) == (b == nil) && bytes.Equal(a, b) }

// absentPath reports whether path does not exist; any other Lstat failure is returned, never read as absence.
func absentPath(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return false, nil
	case os.IsNotExist(err):
		return true, nil
	}
	return false, err
}

// putWithout plans rel rewritten without names' pointer lines; a malformed bank region names the file and plans nothing.
func (b *nativeBatch) putWithout(rel, text string, names []string, preimage []byte) error {
	out, err := indexWithout(text, names)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	b.put(rel, []byte(out), preimage)
	return nil
}

// del plans removing rel on condition of its current bytes.
func (b *nativeBatch) del(rel string, preimage []byte) {
	b.changes = append(b.changes, Change{Rel: filepath.Join(b.prefix, rel), Delete: true, Preimage: preimage, Checked: true})
}

// commit lands the batch in order and returns the memory-Git commit it settled into ("" when nothing changed or
// the dir is outside the store); a divergent preimage names its path and, in the store, leaves the rest committed.
func (b *nativeBatch) commit(subject string) (string, error) {
	if len(b.changes) == 0 {
		return "", nil
	}
	if b.inStore {
		rec, err := commitChangesLocked(subject, b.changes)
		if errors.Is(err, errDivergent) {
			err = fmt.Errorf("%w: %s", err, strings.Join(rec.Divergent, ", "))
		}
		return rec.Commit, err
	}
	for _, c := range b.changes {
		if err := applyChecked(b.base, c); err != nil {
			return "", err
		}
	}
	return "", nil
}

// applyChecked lands one change with the transaction's preimage rules: already at Data (or deleted and absent) is a
// replayed step, any other mismatch is divergence, and a creation is exclusive so no concurrent write is replaced.
func applyChecked(base string, c Change) error {
	abs := filepath.Join(base, c.Rel)
	cur, err := os.ReadFile(abs)
	absent := os.IsNotExist(err)
	if err != nil && !absent {
		return err
	}
	if replayed, err := preimageGate(c, cur, absent); replayed || err != nil {
		return err
	}
	switch {
	case c.Delete:
		return os.Remove(abs)
	case absent:
		return writeExclusive(abs, c.Data)
	}
	return paths.AtomicWrite(abs, c.Data, 0o644)
}

// preimageGate reports a replayed step (already landed) or a divergence error when the live bytes match neither side.
func preimageGate(c Change, cur []byte, absent bool) (replayed bool, err error) {
	switch {
	case c.Delete && absent, !c.Delete && !absent && bytes.Equal(cur, c.Data):
		return true, nil
	case absent != (c.Preimage == nil), !absent && !bytes.Equal(cur, c.Preimage):
		return false, fmt.Errorf("%w: %s", errDivergent, c.Rel)
	}
	return false, nil
}

// indexLinks maps each topic file one index's pointer lines target to its full line; the generated bank
// region is not parsed, so its pointers are never counted, struck, moved or recorded.
func indexLinks(index string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(outsideBankRegion(index), "\n") {
		if target, ok := linkTarget(line); ok {
			out[target] = line
		}
	}
	return out
}

// indexedLinks is every pointer dir's store holds: the injected MEMORY.md plus the recall-only SEARCHABLE.md.
func indexedLinks(dir, index string) (map[string]string, error) {
	raw, err := readIfExists(filepath.Join(dir, NativeSearchableFile))
	if err != nil {
		return nil, err
	}
	return mergedLinks(index, raw), nil
}

// mergedLinks is indexedLinks over SEARCHABLE.md bytes the caller already read; MEMORY.md's line wins a tie.
func mergedLinks(index string, searchable []byte) map[string]string {
	out := indexLinks(index)
	for t, line := range indexLinks(string(searchable)) {
		if _, ok := out[t]; !ok {
			out[t] = line
		}
	}
	return out
}

// linkTarget pulls the `](<target>.md)` a pointer line links to.
func linkTarget(line string) (string, bool) {
	_, after, ok := strings.Cut(line, "](")
	if !ok || !strings.HasPrefix(strings.TrimSpace(line), "-") {
		return "", false
	}
	rest := after
	before0, _, ok0 := strings.Cut(rest, ")")
	if !ok0 {
		return "", false
	}
	t := strings.TrimSpace(before0)
	if !strings.HasSuffix(t, ".md") || strings.Contains(t, "/") {
		return "", false
	}
	return t, true
}

// nativeStats reports dir's injected cost and the two integrity faults, counting only topic files.
func nativeStats(dir string) (nativeStoreStats, error) {
	var st nativeStoreStats
	topics, err := topicFiles(dir)
	if err != nil {
		return st, err
	}
	st.TopicFiles = len(topics)
	present := map[string]bool{}
	for _, p := range topics {
		present[filepath.Base(p)] = true
		if fi, err := os.Stat(p); err == nil {
			st.TopicBytes += fi.Size()
		}
	}
	raw, err := readIfExists(filepath.Join(dir, NativeIndexFile))
	if err != nil {
		return st, err
	}
	if err := checkBankRegion(string(raw)); err != nil {
		return st, fmt.Errorf("%s: %w", NativeIndexFile, err)
	}
	st.IndexBytes = int64(len(raw))
	st.IndexEntries = len(indexLinks(string(raw)))
	st.Dangling, st.Orphans, err = indexFaults(dir, string(raw), present)
	return st, err
}

// indexFaults lists the pointers whose target is no topic or bank file (dangling) and the topics no pointer reaches (orphans).
func indexFaults(dir, raw string, present map[string]bool) (dangling, orphans []string, err error) {
	// A pointer to a typed bank file is valid (not dangling), though the file is never an orphan candidate.
	linkable := maps.Clone(present)
	for _, name := range BankFiles() {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			linkable[name] = true
		}
	}
	links, err := indexedLinks(dir, raw)
	if err != nil {
		return nil, nil, err
	}
	for target := range links {
		if !linkable[target] {
			dangling = append(dangling, target)
		}
	}
	for name := range present {
		if _, ok := links[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(dangling)
	sort.Strings(orphans)
	return dangling, orphans, nil
}

// resolveTopics splits names into those present in base (todo) and "<name>: <absentMsg>" skips; skipped is kept on error.
func resolveTopics(base string, names []string, absentMsg string) (todo, skipped []string, err error) {
	for _, arg := range names {
		name, err := topicName(base, arg)
		if err != nil {
			return todo, skipped, err
		}
		absent, err := absentPath(filepath.Join(base, name))
		if err != nil {
			return todo, skipped, err
		}
		if absent {
			skipped = append(skipped, name+": "+absentMsg)
			continue
		}
		todo = append(todo, name)
	}
	return todo, skipped, nil
}

// archiveNative moves each named topic file out of dir into its archive and strikes its pointer line
// (from whichever of MEMORY.md/SEARCHABLE.md carries it), recording that line verbatim so restoreNative
// is exact rather than reconstructed. Nothing is deleted — the file moves. apply=false plans only.
// A symlink, directory, typed bank file or index name is refused outright (topicName classifies).
func archiveNative(dir string, names []string, apply bool) (nativeResult, error) {
	res := nativeResult{Applied: apply}
	todo, skipped, err := resolveTopics(dir, names, "not in the store")
	res.Skipped = skipped
	if err != nil {
		return res, err
	}
	if len(todo) == 0 || !apply {
		res.Archived = todo
		return res, nil
	}
	err = WithBankLock(BankLockWait, func() error { return archiveNativeLocked(dir, todo, &res) })
	return res, err
}

// nativeArchiveRel is nativeArchiveDir relative to its dir, the form the batch takes.
var nativeArchiveRel = filepath.Join("archive", "native")

// archiveNativeLocked is archiveNative's apply path for a caller that already holds the bank lock: one batch that
// records, strikes, lands, then removes — a replay or a crash leaves an orphan a re-run moves, never a
// dangling pointer. The reconcile repair rides in the same batch rather than as a separate write.
func archiveNativeLocked(dir string, todo []string, res *nativeResult) error {
	ix, err := healedIndex(dir)
	if err != nil {
		return err
	}
	moves, recorded, err := planArchive(dir, todo, mergedLinks(ix.healed, ix.searchable))
	if err != nil {
		return err
	}
	b := newNativeBatch(dir)
	final, err := stageArchive(b, todo, ix, moves, recorded)
	if err != nil {
		return err
	}
	var landed []string
	for _, m := range moves {
		landed = append(landed, m.landed)
	}
	sha, err := b.commit("archive " + strings.Join(todo, ", "))
	if err != nil {
		return err
	}
	res.Archived, res.Commit = landed, sha
	if ix.index == nil {
		return nil
	}
	return landedIndexOverLimit("archived", todo, final)
}

// archiveIndexes are the store's index files, read once before an archive or restore: raw and healed MEMORY.md,
// SEARCHABLE.md and the archive's own record (nil when absent).
type archiveIndexes struct {
	index      []byte
	healed     string
	searchable []byte
	record     []byte
}

// readIfExists reads path; an absent file is empty, any other failure is returned. A var so tests count reads.
var readIfExists = func(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// stageArchive adds the archive's writes to b: the record, both indexes struck, each landed copy, then the removals.
func stageArchive(b *nativeBatch, todo []string, ix archiveIndexes, moves []archiveMove, recorded []string) (final string, err error) {
	if len(recorded) > 0 {
		recRel := filepath.Join(nativeArchiveRel, NativeArchiveIndexFile)
		b.put(recRel, []byte(archiveIndexWith(ix.record, recorded)), ix.record)
	}
	if ix.index != nil {
		if final, err = indexWithout(ix.healed, todo); err != nil {
			return "", fmt.Errorf("%s: %w", NativeIndexFile, err)
		}
		b.put(NativeIndexFile, []byte(final), ix.index)
	}
	if ix.searchable != nil {
		if err := b.putWithout(NativeSearchableFile, string(ix.searchable), todo, ix.searchable); err != nil {
			return "", err
		}
	}
	for _, m := range moves {
		if !m.dedupe {
			b.put(filepath.Join(nativeArchiveRel, m.landed), m.data, nil)
		}
	}
	for _, m := range moves {
		b.del(m.name, m.data)
	}
	return final, nil
}

// landedIndexOverLimit is the loader-limit diagnostic after a batch has landed: the write stays, the caller reports it.
func landedIndexOverLimit(verb string, todo []string, final string) error {
	if err := checkNativeIndexLimits(final); err != nil {
		return fmt.Errorf("%s %s, but the index busts the limit — %w", verb, strings.Join(todo, ", "), err)
	}
	return nil
}

// archiveMove is one planned topic move: name's bytes land under landed in the archive (nothing to write on a dedupe).
type archiveMove struct {
	name, landed string
	data         []byte
	dedupe       bool
}

// planArchive resolves each todo name's landing and its struck pointer line, re-pointed at a stamped landing name
// so restoring that version finds it.
func planArchive(dir string, todo []string, links map[string]string) (moves []archiveMove, recorded []string, err error) {
	archiveDir := nativeArchiveDir(dir)
	for _, name := range todo {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, nil, err
		}
		final, dedupe, err := archiveLanding(data, filepath.Join(archiveDir, name))
		if err != nil {
			return nil, nil, fmt.Errorf("archive %s: %w", name, err)
		}
		if line, ok := links[name]; ok {
			recorded = append(recorded, retargetLine(line, filepath.Base(final)))
		}
		moves = append(moves, archiveMove{name, filepath.Base(final), data, dedupe})
	}
	return moves, recorded, nil
}

// archiveLanding picks where data lands for dst: an equal dst or <stem>@*.md revision dedupes (nothing to write),
// else dst while absent, else <stem>@<UTC stamp>[-n].md — a previous revision is never replaced.
func archiveLanding(data []byte, dst string) (final string, dedupe bool, err error) {
	archiveDir, stem := filepath.Dir(dst), strings.TrimSuffix(dst, ".md")
	revisions := []string{dst}
	ents, err := os.ReadDir(archiveDir)
	if err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), filepath.Base(stem)+"@") && strings.HasSuffix(e.Name(), ".md") {
			revisions = append(revisions, filepath.Join(archiveDir, e.Name()))
		}
	}
	taken := map[string]bool{}
	for _, rev := range revisions {
		cur, err := os.ReadFile(rev)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if bytes.Equal(cur, data) {
			return rev, true, nil
		}
		taken[rev] = true
	}
	if !taken[dst] {
		return dst, false, nil
	}
	base := stem + "@" + time.Now().UTC().Format("20060102T150405Z")
	final = base + ".md"
	for n := 2; taken[final]; n++ {
		final = base + "-" + strconv.Itoa(n) + ".md"
	}
	return final, false, nil
}

// writeExclusive creates abs with data, failing with os.ErrExist rather than replacing anything; a torn write is removed.
func writeExclusive(abs string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errors.Join(werr, os.Remove(abs))
	}
	return nil
}

// retargetLine re-points a pointer line's `](<target>)` at to, the name its bytes landed under.
func retargetLine(line, to string) string {
	open := strings.Index(line, "](") + 2
	closeIdx := open + strings.Index(line[open:], ")")
	return line[:open] + to + line[closeIdx:]
}

// reconcileLines heals a crashed archive/restore in memory: one pointer per store topic both index files omit,
// the verbatim archived line if recorded, else one built from the topic's own frontmatter; rec is the record read.
func reconcileLines(dir, index string, searchable []byte) (missing []string, rec []byte, err error) {
	links := mergedLinks(index, searchable)
	topics, err := topicFiles(dir)
	if err != nil {
		return nil, nil, err
	}
	rec, err = readIfExists(filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile))
	if err != nil {
		return nil, nil, err
	}
	recorded := indexLinks(string(rec))
	for _, p := range topics {
		name := filepath.Base(p)
		if _, ok := links[name]; ok {
			continue
		}
		if line, ok := recorded[name]; ok {
			missing = append(missing, line)
		} else if line, ok := frontmatterPointer(dir, name); ok {
			missing = append(missing, line)
		}
	}
	return missing, rec, nil
}

// healedIndex reads the store's index files once: MEMORY.md (nil when absent) healed with reconcileLines in memory;
// a malformed or over-reserve bank region is the error that stops the caller's rewrite.
func healedIndex(dir string) (archiveIndexes, error) {
	raw, err := readIfExists(filepath.Join(dir, NativeIndexFile))
	if err != nil {
		return archiveIndexes{}, err
	}
	if err := checkBankRegion(string(raw)); err != nil {
		return archiveIndexes{}, fmt.Errorf("%s: %w", NativeIndexFile, err)
	}
	searchable, err := readIfExists(filepath.Join(dir, NativeSearchableFile))
	if err != nil {
		return archiveIndexes{}, err
	}
	missing, rec, err := reconcileLines(dir, string(raw), searchable)
	if err != nil {
		return archiveIndexes{}, err
	}
	return archiveIndexes{index: raw, healed: indexWithLines(string(raw), missing), searchable: searchable, record: rec}, nil
}

// frontmatterPointer builds `- [name](file.md) — description` from a topic file's frontmatter; false without a name.
func frontmatterPointer(dir, file string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "", false
	}
	name, desc, _, ok := topicFrontmatter(string(raw))
	if !ok {
		return "", false
	}
	line := "- [" + name + "](" + file + ")"
	if desc != "" {
		line += " — " + desc
	}
	return line, true
}

// linesFor returns the recorded pointer line of each name that has one, in names order.
func linesFor(lines map[string]string, names []string) []string {
	var out []string
	for _, name := range names {
		if line, ok := lines[name]; ok {
			out = append(out, line)
		}
	}
	return out
}

// indexWithLines is index with pointer lines appended; no lines leaves it byte-for-byte.
func indexWithLines(index string, lines []string) string {
	if len(lines) == 0 {
		return index
	}
	body := strings.TrimRight(index, "\n")
	if body != "" {
		body += "\n"
	}
	return body + strings.Join(lines, "\n") + "\n"
}

// archiveIndexWith is the archive's own record (nil = not yet written) with struck pointer lines appended.
func archiveIndexWith(rec []byte, lines []string) string {
	body := string(rec)
	if rec == nil {
		body = "# Archived native memories\n\n> Struck MEMORY.md/SEARCHABLE.md pointer lines, kept verbatim: " +
			"`claude-code memory archive --restore <name> --apply` puts one back.\n\n"
	}
	if !strings.HasSuffix(body, "\n") && body != "" {
		body += "\n"
	}
	return body + strings.Join(lines, "\n") + "\n"
}

// indexWithout drops every pointer line targeting one of names; the bank region is kept byte-for-byte, and a
// malformed region rejects the rewrite rather than risking its lines.
func indexWithout(index string, names []string) (string, error) {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	region, err := bankRegionLines(index)
	if err != nil {
		return "", err
	}
	var kept []string
	for i, line := range strings.Split(index, "\n") {
		if t, ok := linkTarget(line); ok && drop[t] && !region[i] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}
