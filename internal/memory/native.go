package memory

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textsim"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// NativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func NativeDirs(root string) []string {
	if CutOver() { // native shares the repo's store dir; the home tier holds the user-wide notes
		out := []string{homeStoreDir()}
		if d := Dir(root); !slices.Contains(out, d) {
			out = append(out, d)
		}
		return out
	}
	roots := []string{paths.ConfigDir(), root}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil && root != "" {
		roots = append(roots, GitToplevel(root)) // a subdir's sessions file under the repo top; forks only off-toplevel
	}
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		if d := filepath.Join(paths.ConfigDir(), "projects", ProjectSlug(r), "memory"); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// NativeDir is root's own native auto-memory dir — the one `memory archive` acts on, as opposed to
// NativeDirs' extra fallback entries (the harness's own notes, a non-toplevel subdir's parent) that
// search reads but archive must not touch.
func NativeDir(root string) string {
	if CutOver() {
		return Dir(root)
	}
	return filepath.Join(paths.ConfigDir(), "projects", ProjectSlug(root), "memory")
}

// ProjectSlug mirrors Claude Code's projects/<slug> naming: every non-alphanumeric byte becomes '-'.
func ProjectSlug(root string) string {
	b := []byte(filepath.Clean(root))
	for i, c := range b {
		if ('a' > c || c > 'z') && ('A' > c || c > 'Z') && ('0' > c || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Native auto-memory's MEMORY.md is injected EVERY session, so its size is a per-turn cost. SEARCHABLE.md
// holds pointer lines moved out of that injected core — recall finds them by content, never injected.
const (
	NativeIndexFile        = "MEMORY.md"     // the injected index of `- [Title](topic.md) — hook` lines
	NativeSearchableFile   = "SEARCHABLE.md" // pointer lines kept OUT of the injected index; recall finds the topics
	NativeArchiveIndexFile = "INDEX.md"      // struck pointer lines, recorded verbatim so restore is exact
	NativeCoreCap          = 20              // max pointer lines SplitNativeIndex leaves injected in MEMORY.md
)

// topicPaths lists dir's KindTopic files; indexes, the catalog and typed bank files are not topics.
func topicPaths(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if classifyMemoryFile(dir, e.Name()) == KindTopic {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
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
	if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
		return name, nil
	}
	if k := classifyMemoryFile(dir, name); !k.Archivable() {
		return "", fmt.Errorf("%q: %s, not an archivable topic", arg, k)
	}
	return name, nil
}

// NativeArchiveDir is dir's own cold tier for archived topics and their pointer records (plan §3.2: dir/archive/native).
func NativeArchiveDir(dir string) string {
	return filepath.Join(dir, nativeArchiveRel)
}

// NativeStoreStats reports dir's injected cost plus two integrity faults across MEMORY.md + SEARCHABLE.md: a
// topic no pointer names (orphan), and a pointer whose target is gone (dangling — an answer that cannot open).
type NativeStoreStats struct {
	TopicFiles   int   // flat topic .md files (the store's own index files excluded)
	IndexEntries int   // `- [Title](topic.md)` lines in MEMORY.md — the part paid on EVERY turn
	IndexBytes   int64 // MEMORY.md's size
	TopicBytes   int64 // topic files' total size — paid only when one is recalled
	Orphans      []string
	Dangling     []string
}

// NativeResult reports one archive/restore pass; Skipped carries the reason per rejected name.
type NativeResult struct {
	Archived []string
	Restored []string
	Skipped  []string
	Applied  bool
	Commit   string // memory-Git commit the applied pass settled into; empty for a dry run or a legacy dir outside the store
}

// nativeBatch plans dir-relative checked changes, then lands them in one pass: via the store transaction (journal,
// settle, commit) when dir is inside the store, else applied directly with the same preimage rules (legacy dir, no repo).
type nativeBatch struct {
	base, prefix string // base is the store (prefix = dir's path inside it) or dir itself for a legacy dir
	inStore      bool
	changes      []Change
}

func newNativeBatch(dir string) *nativeBatch {
	if rel, ok := InsideStore(dir); ok {
		return &nativeBatch{base: StoreDir(), prefix: filepath.FromSlash(rel), inStore: true}
	}
	return &nativeBatch{base: dir}
}

// put plans writing data at rel on condition of preimage (nil = must not exist); equal bytes plan nothing.
func (b *nativeBatch) put(rel string, data, preimage []byte) {
	if !bytes.Equal(data, preimage) {
		b.changes = append(b.changes, Change{Rel: filepath.Join(b.prefix, rel), Data: data, Preimage: preimage, Checked: true})
	}
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
// the dir is a legacy one); a divergent preimage names its path and, in the store, leaves the rest committed.
func (b *nativeBatch) commit(subject string) (string, error) {
	if len(b.changes) == 0 {
		return "", nil
	}
	if b.inStore {
		rec, err := commitChangesLocked(subject, b.changes)
		if errors.Is(err, ErrDivergent) {
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
	switch {
	case c.Delete && absent, !c.Delete && !absent && bytes.Equal(cur, c.Data):
		return nil
	case absent != (c.Preimage == nil), !absent && !bytes.Equal(cur, c.Preimage):
		return fmt.Errorf("%w: %s", ErrDivergent, c.Rel)
	case c.Delete:
		return os.Remove(abs)
	case absent:
		return writeExclusive(abs, c.Data)
	}
	return paths.AtomicWrite(abs, c.Data, 0o644)
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
func indexedLinks(dir, index string) map[string]string {
	out := indexLinks(index)
	if raw, err := os.ReadFile(filepath.Join(dir, NativeSearchableFile)); err == nil {
		for t, line := range indexLinks(string(raw)) {
			if _, ok := out[t]; !ok {
				out[t] = line
			}
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

// NativeStats reports dir's injected cost and the two integrity faults, counting only topic files.
func NativeStats(dir string) (NativeStoreStats, error) {
	var st NativeStoreStats
	topics := topicPaths(dir)
	st.TopicFiles = len(topics)
	present := map[string]bool{}
	for _, p := range topics {
		present[filepath.Base(p)] = true
		if fi, err := os.Stat(p); err == nil {
			st.TopicBytes += fi.Size()
		}
	}
	// A pointer to a typed bank file is valid (not dangling), though the file is never an orphan candidate.
	linkable := maps.Clone(present)
	for _, name := range bankFiles {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			linkable[name] = true
		}
	}
	indexPath := filepath.Join(dir, NativeIndexFile)
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := checkBankRegion(string(raw)); err != nil {
		return st, fmt.Errorf("%s: %w", NativeIndexFile, err)
	}
	st.IndexBytes = int64(len(raw))
	st.IndexEntries = len(indexLinks(string(raw)))
	links := indexedLinks(dir, string(raw))
	for target := range links {
		if !linkable[target] {
			st.Dangling = append(st.Dangling, target)
		}
	}
	for name := range present {
		if _, ok := links[name]; !ok {
			st.Orphans = append(st.Orphans, name)
		}
	}
	sort.Strings(st.Dangling)
	sort.Strings(st.Orphans)
	return st, nil
}

// ArchiveNative moves each named topic file out of dir into its archive and strikes its pointer line
// (from whichever of MEMORY.md/SEARCHABLE.md carries it), recording that line verbatim so RestoreNative
// is exact rather than reconstructed. Nothing is deleted — the file moves. apply=false plans only.
// A symlink, directory, typed bank file or index name is refused outright (topicName classifies).
func ArchiveNative(dir string, names []string, apply bool) (NativeResult, error) {
	res := NativeResult{Applied: apply}
	var todo []string
	for _, arg := range names {
		name, err := topicName(dir, arg)
		if err != nil {
			return res, err
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			res.Skipped = append(res.Skipped, name+": not in the store")
			continue
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 || !apply {
		res.Archived = todo
		return res, nil
	}
	err := WithWritableBankLock(BankLockWait, func() error { return archiveNativeLocked(dir, todo, &res) })
	return res, err
}

// nativeArchiveRel is NativeArchiveDir relative to its dir, the form the batch takes.
var nativeArchiveRel = filepath.Join("archive", "native")

// archiveNativeLocked is ArchiveNative's apply path for a caller that already holds the bank lock: one batch that
// records, strikes, lands, then removes — a replay or a legacy crash leaves an orphan a re-run moves, never a
// dangling pointer. The reconcile repair rides in the same batch rather than as a separate write.
func archiveNativeLocked(dir string, todo []string, res *NativeResult) error {
	index, healed, err := healedIndex(dir)
	if err != nil {
		return err
	}
	searchable, _ := os.ReadFile(filepath.Join(dir, NativeSearchableFile))
	moves, recorded, err := planArchive(dir, todo, indexedLinks(dir, healed))
	if err != nil {
		return err
	}
	b := newNativeBatch(dir)
	recRel := filepath.Join(nativeArchiveRel, NativeArchiveIndexFile)
	rec, _ := os.ReadFile(filepath.Join(dir, recRel))
	if len(recorded) > 0 {
		b.put(recRel, []byte(archiveIndexWith(rec, recorded)), rec)
	}
	var final string
	if index != nil {
		if final, err = indexWithout(healed, todo); err != nil {
			return fmt.Errorf("%s: %w", NativeIndexFile, err)
		}
		b.put(NativeIndexFile, []byte(final), index)
	}
	if searchable != nil {
		if err := b.putWithout(NativeSearchableFile, string(searchable), todo, searchable); err != nil {
			return err
		}
	}
	var landed []string
	for _, m := range moves {
		if !m.dedupe {
			b.put(filepath.Join(nativeArchiveRel, m.landed), m.data, nil)
		}
		landed = append(landed, m.landed)
	}
	for _, m := range moves {
		b.del(m.name, m.data)
	}
	sha, err := b.commit("archive " + strings.Join(todo, ", "))
	if err != nil {
		return err
	}
	res.Archived, res.Commit = landed, sha
	if index == nil {
		return nil
	}
	return landedIndexOverLimit("archived", todo, final)
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
	archiveDir := NativeArchiveDir(dir)
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
	ents, _ := os.ReadDir(archiveDir)
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
// the verbatim archived line if recorded, else one built from the topic's own frontmatter.
func reconcileLines(dir, index string) []string {
	links := indexedLinks(dir, index)
	rec, _ := os.ReadFile(filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile))
	recorded := indexLinks(string(rec))
	var missing []string
	for _, p := range topicPaths(dir) {
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
	return missing
}

// healedIndex is MEMORY.md's bytes (nil when absent) and its text with reconcileLines appended in memory;
// a malformed or over-reserve bank region is the error that stops the caller's rewrite.
func healedIndex(dir string) (raw []byte, healed string, err error) {
	raw, err = os.ReadFile(filepath.Join(dir, NativeIndexFile))
	if err != nil {
		return nil, "", nil
	}
	if err := checkBankRegion(string(raw)); err != nil {
		return nil, "", fmt.Errorf("%s: %w", NativeIndexFile, err)
	}
	return raw, indexWithLines(string(raw), reconcileLines(dir, string(raw))), nil
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

// RestoreNative reverses ArchiveNative: the topic file moves back and its recorded pointer line is re-appended
// verbatim — to MEMORY.md while the injected core has room, else to SEARCHABLE.md.
// The archived copy is classified in place, so a typed bank file or index name can never be restored
// over a live one; a live file with different bytes aborts the whole pass before anything moves.
func RestoreNative(dir string, names []string, apply bool) (NativeResult, error) {
	res := NativeResult{Applied: apply}
	archiveDir := NativeArchiveDir(dir)
	var todo []string
	for _, arg := range names {
		name, err := topicName(archiveDir, arg)
		if err != nil {
			return res, err
		}
		if _, err := os.Lstat(filepath.Join(archiveDir, name)); err != nil {
			res.Skipped = append(res.Skipped, name+": not in the archive")
			continue
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 || !apply {
		res.Restored = todo
		return res, nil
	}
	err := WithWritableBankLock(BankLockWait, func() error { return restoreNativeLocked(dir, todo, &res) })
	return res, err
}

// restoreNativeLocked is RestoreNative's apply path for a caller that already holds the bank lock: one batch that
// lands the files, re-points them, removes the archive copies, then strikes the record. An equal live copy (a
// crashed restore) is kept and only the archive copy goes; the reconcile repair rides in the same batch.
func restoreNativeLocked(dir string, todo []string, res *NativeResult) error {
	index, healed, err := healedIndex(dir)
	if err != nil {
		return err
	}
	recRel := filepath.Join(nativeArchiveRel, NativeArchiveIndexFile)
	rec, _ := os.ReadFile(filepath.Join(dir, recRel))
	b := newNativeBatch(dir)
	archived := map[string][]byte{}
	var moved []string
	for _, name := range todo {
		data, err := os.ReadFile(filepath.Join(dir, nativeArchiveRel, name))
		if err != nil {
			return err
		}
		live, err := liveCollision(filepath.Join(dir, name), data)
		if err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		if !live {
			b.put(name, data, nil)
			moved = append(moved, name)
		}
		archived[name] = data
	}
	lines := linesFor(indexLinks(string(rec)), moved)
	if len(indexLinks(healed)) >= NativeCoreCap {
		// The injected core is full: the restored pointers stay recall-only, and the core takes only its repair.
		searchable, _ := os.ReadFile(filepath.Join(dir, NativeSearchableFile))
		b.put(NativeSearchableFile, []byte(indexWithLines(string(searchable), lines)), searchable)
		lines = nil
	}
	final := indexWithLines(healed, lines)
	b.put(NativeIndexFile, []byte(final), index)
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

// SplitNativeIndex trims MEMORY.md to at most keepLines pointer lines, moving the oldest unpinned ones
// (file order = oldest first: Claude Code appends new memories at the bottom) to SEARCHABLE.md. A line
// carrying a `<!-- keep -->` marker never moves; pins claim the budget first, recency fills the rest.
// Returns the number of lines moved. apply is the caller's job — this always writes when there is work.
func SplitNativeIndex(dir string, keepLines int) (int, error) {
	var moved int
	err := WithWritableBankLock(BankLockWait, func() (err error) {
		moved, err = splitNativeIndexLocked(dir, keepLines)
		return err
	})
	return moved, err
}

// splitNativeIndexLocked is SplitNativeIndex for a caller that already holds the bank lock. Lines inside
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
	region, _ := bankRegionLines(string(raw)) // same scan checkBankRegion just passed
	var pinnedCount int
	var unpinnedIdx []int
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
	budget := max(keepLines-pinnedCount, 0)
	if len(unpinnedIdx) <= budget {
		return 0, checkNativeIndexLimits(string(raw))
	}
	drop := unpinnedIdx[:len(unpinnedIdx)-budget] // oldest unpinned pointer lines, file order preserved
	dropSet := map[int]bool{}
	var overflow []string
	for _, i := range drop {
		dropSet[i] = true
		overflow = append(overflow, lines[i])
	}
	var kept []string
	for i, line := range lines {
		if !dropSet[i] {
			kept = append(kept, line)
		}
	}
	// One batch, SEARCHABLE.md first: a legacy crash between the two leaves a duplicated pointer, never a lost one.
	b := newNativeBatch(dir)
	searchable, _ := os.ReadFile(filepath.Join(dir, NativeSearchableFile))
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

// NativeDupe is one near-duplicate cluster: Keep is the survivor (the longest body — the best-evidenced
// carrier), Losers the rest. Similarity is Jaccard over the frontmatter description + body tokens.
type NativeDupe struct {
	Keep   string
	Losers []string
	Score  float64
}

// NativeDupes clusters dir's topic files whose bodies overlap at or above threshold. Report-only: naming
// a survivor is a judgement call about which one carries the better evidence, so nothing moves here.
func NativeDupes(dir string, threshold float64) ([]NativeDupe, error) {
	topics := topicPaths(dir)
	sets := map[string]map[string]bool{}
	size := map[string]int{}
	var names []string
	for _, p := range topics {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		name := filepath.Base(p)
		names = append(names, name)
		sets[name] = textsim.TokenSet(string(b))
		size[name] = len(b)
	}
	seen := map[string]bool{}
	var out []NativeDupe
	for i, a := range names {
		if seen[a] {
			continue
		}
		cluster := NativeDupe{Keep: a}
		for _, b := range names[i+1:] {
			if seen[b] {
				continue
			}
			if s := textsim.Jaccard(sets[a], sets[b]); s >= threshold {
				cluster.Losers = append(cluster.Losers, b)
				seen[b] = true
				if s > cluster.Score {
					cluster.Score = s
				}
			}
		}
		if len(cluster.Losers) == 0 {
			continue
		}
		// The longest body wins: a memory carrying more evidence is the better carrier to merge into.
		for _, c := range append([]string{cluster.Keep}, cluster.Losers...) {
			if size[c] > size[cluster.Keep] {
				cluster.Keep = c
			}
		}
		cluster.Losers = without(append([]string{a}, cluster.Losers...), cluster.Keep)
		seen[cluster.Keep] = true
		out = append(out, cluster)
	}
	return out, nil
}

// without returns names minus drop, order preserved.
func without(names []string, drop string) []string {
	var out []string
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// topicFrontmatter splits a topic file into its name, description and body; false without a closed block and a name.
// The closing fence must start its own line (markdown.StripFrontMatter), so a name/description value that merely
// contains the literal bytes "---" can't be mistaken for it.
func topicFrontmatter(raw string) (name, desc, body string, ok bool) {
	prefix, content := markdown.StripFrontMatter([]byte(raw))
	if prefix == nil {
		return "", "", "", false // an absent or unclosed block is body text, not frontmatter
	}
	for _, line := range strings.Split(string(prefix), "\n") {
		if v, found := strings.CutPrefix(line, "name:"); found {
			name = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, found := strings.CutPrefix(line, "description:"); found {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if name == "" {
		return "", "", "", false
	}
	return name, desc, strings.TrimSpace(string(content)), true
}
