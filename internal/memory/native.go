package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textsim"
)

// NativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func NativeDirs(root string) []string {
	roots := []string{paths.ConfigDir(), root}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil && root != "" {
		roots = append(roots, GitToplevel(root)) // a subdir's sessions file under the repo top; forks only off-toplevel
	}
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		if d := filepath.Join(paths.ConfigDir(), "projects", projectSlug(r), "memory"); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// NativeDir is root's own native auto-memory dir — the one `memory archive` acts on, as opposed to
// NativeDirs' extra fallback entries (the harness's own notes, a non-toplevel subdir's parent) that
// search reads but archive must not touch.
func NativeDir(root string) string {
	return filepath.Join(paths.ConfigDir(), "projects", projectSlug(root), "memory")
}

// projectSlug mirrors Claude Code's projects/<slug> naming: every non-alphanumeric byte becomes '-'.
func projectSlug(root string) string {
	b := []byte(filepath.Clean(root))
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
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

// reservedNative are store files that share the directory with topic files but are not topics
// themselves: the injected index, the recall-only pointer file, and the archive's own line record.
var reservedNative = map[string]bool{NativeIndexFile: true, NativeSearchableFile: true, NativeArchiveIndexFile: true}

// isTopicFile reports whether name (a bare filename in a native store dir) is a topic file.
func isTopicFile(name string) bool {
	return strings.HasSuffix(name, ".md") && !reservedNative[name]
}

// topicPaths lists dir's topic files; MEMORY.md/SEARCHABLE.md/INDEX.md are pointer indexes, not topics.
func topicPaths(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if !e.IsDir() && isTopicFile(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// topicName normalizes a user-supplied name to "<slug>.md", refusing anything that is not a plain
// topic file: a path, a traversal, or one of the store's own index files. Callers pass this straight
// to a move, so letting a traversal or an index filename through here would delete store state.
func topicName(arg string) (string, error) {
	name := strings.TrimSpace(arg)
	if name == "" {
		return "", fmt.Errorf("empty name")
	}
	if name != filepath.Base(name) || strings.Contains(name, string(filepath.Separator)) {
		return "", fmt.Errorf("%q: not a bare topic name", arg)
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	if !isTopicFile(name) {
		return "", fmt.Errorf("%q: not a topic file (%s, %s and %s are not archivable)", arg,
			NativeIndexFile, NativeSearchableFile, NativeArchiveIndexFile)
	}
	return name, nil
}

// NativeArchiveDir is where dir's archived topic files land, tagged by dir's own project slug so
// archives from different projects' native stores never collide in one directory.
func NativeArchiveDir(dir string) string {
	return filepath.Join(GlobalDir(), "archive", "native", nativeProjectSlug(dir))
}

// nativeProjectSlug pulls <slug> out of ".../projects/<slug>/memory"; a dir of another shape falls
// back to a sanitized form of the whole path so the archive still lands somewhere stable.
func nativeProjectSlug(dir string) string {
	if filepath.Base(dir) == "memory" {
		if slug := filepath.Base(filepath.Dir(dir)); slug != "." && slug != string(filepath.Separator) {
			return slug
		}
	}
	return paths.Slug(dir, "._-", 40, "default")
}

// nativeLockPath serializes one dir's archive/restore/split writers.
func nativeLockPath(dir string) string { return filepath.Join(dir, ".archive") }

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
}

// indexLinks maps each topic file one index's pointer lines target to its full line.
func indexLinks(index string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(index, "\n") {
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
	i := strings.Index(line, "](")
	if i < 0 || !strings.HasPrefix(strings.TrimSpace(line), "-") {
		return "", false
	}
	rest := line[i+2:]
	j := strings.Index(rest, ")")
	if j < 0 {
		return "", false
	}
	t := strings.TrimSpace(rest[:j])
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
	indexPath := filepath.Join(dir, NativeIndexFile)
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	st.IndexBytes = int64(len(raw))
	st.IndexEntries = len(indexLinks(string(raw)))
	links := indexedLinks(dir, string(raw))
	for target := range links {
		if !present[target] {
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
func ArchiveNative(dir string, names []string, apply bool) (NativeResult, error) {
	res := NativeResult{Applied: apply}
	var todo []string
	for _, arg := range names {
		name, err := topicName(arg)
		if err != nil {
			return res, err
		}
		fi, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil {
			res.Skipped = append(res.Skipped, name+": not in the store")
			continue
		}
		// A .md-suffixed DIRECTORY passes every name check; renaming it would move a whole subtree.
		if fi.IsDir() {
			return res, fmt.Errorf("%q: is a directory, not a topic file", name)
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 || !apply {
		res.Archived = todo
		return res, nil
	}
	archiveDir := NativeArchiveDir(dir)
	err := paths.WithFileLock(nativeLockPath(dir), func() error {
		if err := ReconcileNative(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return err
		}
		indexPath := filepath.Join(dir, NativeIndexFile)
		index, _ := os.ReadFile(indexPath)
		searchablePath := filepath.Join(dir, NativeSearchableFile)
		searchable, _ := os.ReadFile(searchablePath)
		recorded := linesFor(indexedLinks(dir, string(index)), todo)
		// Record, strike, then move: a crash between steps leaves an orphan a re-run moves, never a dangling pointer.
		if len(recorded) > 0 {
			if err := appendArchiveIndex(archiveDir, recorded); err != nil {
				return err
			}
		}
		if err := rewriteIndexWithout(indexPath, string(index), todo); err != nil {
			return err
		}
		if err := rewriteIndexWithout(searchablePath, string(searchable), todo); err != nil {
			return err
		}
		for _, name := range todo {
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(archiveDir, name)); err != nil {
				// The archive record above already holds every todo entry's line verbatim (whichever
				// file it came from), so re-pointing the ones still here is a reconcile, not a rebuild.
				return errors.Join(fmt.Errorf("archive %s: %w", name, err), ReconcileNative(dir))
			}
			res.Archived = append(res.Archived, name)
		}
		return nil
	})
	return res, err
}

// ReconcileNative heals a crashed archive/restore: it re-points every store topic file that MEMORY.md
// and SEARCHABLE.md both omit, using the verbatim archived line if recorded, else one built from the
// topic's own frontmatter. Idempotent — safe to run before every archive/restore, and on its own.
func ReconcileNative(dir string) error {
	indexPath := filepath.Join(dir, NativeIndexFile)
	index, err := os.ReadFile(indexPath)
	if err != nil {
		return nil // no index to heal: creating MEMORY.md is Claude Code's call, not a repair
	}
	topics := topicPaths(dir)
	links := indexedLinks(dir, string(index))
	rec, _ := os.ReadFile(filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile))
	recorded := indexLinks(string(rec))
	var missing []string
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
	return appendIndexLines(indexPath, missing)
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

// appendIndexLines appends pointer lines to an index file atomically; a torn MEMORY.md is injected verbatim.
func appendIndexLines(path string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	index, _ := os.ReadFile(path)
	body := strings.TrimRight(string(index), "\n")
	if body != "" {
		body += "\n"
	}
	return paths.AtomicWrite(path, []byte(body+strings.Join(lines, "\n")+"\n"), 0o644)
}

// appendArchiveIndex records struck pointer lines in archiveDir's own index.
func appendArchiveIndex(archiveDir string, lines []string) error {
	path := filepath.Join(archiveDir, NativeArchiveIndexFile)
	body := ""
	if b, err := os.ReadFile(path); err == nil {
		body = string(b)
	} else {
		body = "# Archived native memories\n\n> Struck MEMORY.md/SEARCHABLE.md pointer lines, kept verbatim: " +
			"`claude-code memory archive --restore <name> --apply` puts one back.\n\n"
	}
	if !strings.HasSuffix(body, "\n") && body != "" {
		body += "\n"
	}
	return paths.AtomicWrite(path, []byte(body+strings.Join(lines, "\n")+"\n"), 0o644)
}

// rewriteIndexWithout drops every pointer line targeting one of names.
func rewriteIndexWithout(path, index string, names []string) error {
	if index == "" {
		return nil
	}
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	var kept []string
	for _, line := range strings.Split(index, "\n") {
		if t, ok := linkTarget(line); ok && drop[t] {
			continue
		}
		kept = append(kept, line)
	}
	return paths.AtomicWrite(path, []byte(strings.Join(kept, "\n")), 0o644)
}

// RestoreNative reverses ArchiveNative: the topic file moves back and its recorded pointer line is re-appended
// verbatim — to MEMORY.md while the injected core has room, else to SEARCHABLE.md.
func RestoreNative(dir string, names []string, apply bool) (NativeResult, error) {
	res := NativeResult{Applied: apply}
	archiveDir := NativeArchiveDir(dir)
	var todo []string
	for _, arg := range names {
		name, err := topicName(arg)
		if err != nil {
			return res, err
		}
		fi, statErr := os.Stat(filepath.Join(archiveDir, name))
		if statErr != nil {
			res.Skipped = append(res.Skipped, name+": not in the archive")
			continue
		}
		if fi.IsDir() {
			return res, fmt.Errorf("%q: is a directory, not a topic file", name)
		}
		todo = append(todo, name)
	}
	if len(todo) == 0 || !apply {
		res.Restored = todo
		return res, nil
	}
	err := paths.WithFileLock(nativeLockPath(dir), func() error {
		if err := ReconcileNative(dir); err != nil {
			return err
		}
		recPath := filepath.Join(archiveDir, NativeArchiveIndexFile)
		rec, _ := os.ReadFile(recPath)
		recLines := indexLinks(string(rec))
		// A mid-loop rename failure still indexes what already moved, so no restored file is left orphaned.
		var moveErr error
		for _, name := range todo {
			if err := os.Rename(filepath.Join(archiveDir, name), filepath.Join(dir, name)); err != nil {
				moveErr = fmt.Errorf("restore %s: %w", name, err)
				break
			}
			res.Restored = append(res.Restored, name)
		}
		target := NativeIndexFile
		if len(indexLinks(readIndexFile(filepath.Join(dir, NativeIndexFile)))) >= NativeCoreCap {
			target = NativeSearchableFile // the injected core is full: restore stays recall-only
		}
		if err := appendIndexLines(filepath.Join(dir, target), linesFor(recLines, res.Restored)); err != nil {
			return errors.Join(moveErr, err)
		}
		if len(res.Restored) == 0 {
			return moveErr
		}
		return errors.Join(moveErr, rewriteIndexWithout(recPath, string(rec), res.Restored))
	})
	return res, err
}

// SplitNativeIndex trims MEMORY.md to at most keepLines pointer lines, moving the oldest unpinned ones
// (file order = oldest first: Claude Code appends new memories at the bottom) to SEARCHABLE.md. A line
// carrying a `<!-- keep -->` marker never moves; pins claim the budget first, recency fills the rest.
// Returns the number of lines moved. apply is the caller's job — this always writes when there is work.
func SplitNativeIndex(dir string, keepLines int) (int, error) {
	indexPath := filepath.Join(dir, NativeIndexFile)
	var moved int
	err := paths.WithFileLock(nativeLockPath(dir), func() error {
		raw, err := os.ReadFile(indexPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		lines := strings.Split(string(raw), "\n")
		var pointerCount, pinnedCount int
		var unpinnedIdx []int
		for i, line := range lines {
			if _, ok := linkTarget(line); !ok {
				continue
			}
			pointerCount++
			if strings.Contains(line, "<!-- keep -->") {
				pinnedCount++
			} else {
				unpinnedIdx = append(unpinnedIdx, i)
			}
		}
		budget := keepLines - pinnedCount
		if budget < 0 {
			budget = 0
		}
		if len(unpinnedIdx) <= budget {
			return nil
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
		if err := appendIndexLines(filepath.Join(dir, NativeSearchableFile), overflow); err != nil {
			return err
		}
		if err := paths.AtomicWrite(indexPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
			return err
		}
		moved = len(overflow)
		return nil
	})
	return moved, err
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
func topicFrontmatter(raw string) (name, desc, body string, ok bool) {
	lines := strings.Split(raw, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", "", "", false
	}
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			if name == "" {
				return "", "", "", false
			}
			return name, desc, strings.TrimSpace(strings.Join(lines[i+2:], "\n")), true
		}
		if v, found := strings.CutPrefix(line, "name:"); found {
			name = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, found := strings.CutPrefix(line, "description:"); found {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return "", "", "", false // an unclosed block is body text, not frontmatter
}

// readIndexFile is a pointer index's text; an absent file is an empty index.
func readIndexFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
