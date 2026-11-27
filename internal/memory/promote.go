package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/paths"
)

// archiveCaps are the per-file caps that trigger archiving; sectionFiles split per section instead of per entry.
var archiveCaps = map[string]int{
	"conventions.md":   fileCaps["conventions.md"],
	"decisionLog.md":   fileCaps["decisionLog.md"],
	"activeContext.md": fileCaps["activeContext.md"],
	"progress.md":      fileCaps["progress.md"],
}

// sectionFiles are the replace-each-session files: every "## " section stays, each keeps its top (newest) bullets.
var sectionFiles = map[string]bool{"activeContext.md": true, "progress.md": true}

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

// archivePath is the archive/<name>-<yyyy-mm>.md destination of archiveFile.
func archivePath(name string, now time.Time) string {
	return paths.MemorySub("archive", strings.TrimSuffix(name, ".md")+"-"+now.Format("2006-01")+".md")
}

// overflowSplit splits off the oldest blocks of file base's body beyond capLines; an in-cap body has no overflow.
func overflowSplit(base, body string, capLines int) (header string, keep, overflow []string) {
	if logicalLineCount(body) <= capLines {
		return "", nil, nil
	}
	return docOverflow(normaliseSections(ParseDoc(base, []byte(body))), capLines)
}

// normaliseSections folds repeated headings and their contents into the first section of the same file.
func normaliseSections(d Doc) Doc {
	if !sectionFiles[d.Name] {
		return d
	}
	out := d
	out.Entries = nil
	first := map[string]int{}
	at := -1
	for _, e := range d.Entries {
		if e.Kind == KindHeading {
			e = canonicalSectionEntry(d.Name, e)
			if i, ok := first[e.Lines[0]]; ok {
				at = i
				mergeSectionIntro(&out, i, e.Lines[1:])
				continue
			}
			at = len(out.Entries)
			first[e.Lines[0]] = at
		}
		if at >= 0 && at != len(out.Entries) && e.Kind == KindBullet {
			end := sectionEnd(out.Entries, at)
			out.Entries = slices.Insert(out.Entries, end, e)
			for h, i := range first {
				if i >= end {
					first[h] = i + 1
				}
			}
			continue
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

func canonicalSectionEntry(name string, e DocEntry) DocEntry {
	for _, canonical := range sectionsOf[strings.TrimSuffix(name, ".md")] {
		if strings.HasPrefix(e.Lines[0], "## "+canonical+" (") && strings.HasSuffix(e.Lines[0], ")") {
			e.Lines = slices.Clone(e.Lines)
			e.Lines[0] = "## " + canonical
			break
		}
	}
	return e
}

func sectionEnd(entries []DocEntry, at int) int {
	end := at + 1
	for end < len(entries) && entries[end].Kind != KindHeading {
		end++
	}
	return end
}

func mergeSectionIntro(d *Doc, at int, lines []string) {
	end := sectionEnd(d.Entries, at)
	last := at
	if end > at+1 {
		last = end - 1
	}
	d.Entries[last].Lines = append(d.Entries[last].Lines, lines...)
}

// docBlocks projects a parsed Doc to the legacy split/rejoin block shape without parsing its bytes again.
func docBlocks(d Doc, sections bool) (string, []string) {
	prefix := slices.Clone(d.Front)
	if d.Header != "" {
		prefix = append(prefix, d.Header)
	}
	prefix = append(prefix, d.Hidden...)
	header := strings.TrimRight(strings.Join(prefix, "\n"), "\n")
	var blocks []string
	for _, e := range d.Entries {
		part := strings.TrimRight(strings.Join(e.Lines, "\n"), "\n")
		if sections && e.Kind == KindBullet && len(blocks) > 0 {
			blocks[len(blocks)-1] += "\n" + part
		} else {
			blocks = append(blocks, part)
		}
	}
	return header, blocks
}

// partitionDoc removes overflow entries; the retained text is the Doc minus them, rendered as the store renders it.
func partitionDoc(d Doc, keep []string) (string, []string) {
	kept := keptEntries(d, keep)
	retained, moved := d, []string(nil)
	retained.Entries = nil
	heading, group := "", -1
	for i, e := range d.Entries {
		raw := e.block()
		if i+1 < len(d.Entries) {
			raw += "\n"
		}
		if sectionFiles[d.Name] && e.Kind == KindHeading {
			heading, group = e.Lines[0], -1
		}
		if kept[i] {
			retained.Entries = append(retained.Entries, e)
			continue
		}
		if !sectionFiles[d.Name] {
			moved = append(moved, raw)
			continue
		}
		if group < 0 {
			group = len(moved)
			moved = append(moved, overflowBlock(d.Name, heading, ""))
		}
		moved[group] += raw
	}
	return string(retained.Render()), moved // the import that moves those rows leaves exactly these bytes
}

// keptEntries marks the entries keep names, claiming duplicates in the order the split kept them: newest first.
func keptEntries(d Doc, keep []string) map[int]bool {
	wanted := map[string]int{}
	for _, k := range entryKeys(ParseDoc(d.Name, []byte(strings.Join(keep, "\n")))) {
		wanted[k]++
	}
	keys, order := entryKeys(d), d.OldestFirst(0, len(d.Entries))
	slices.Reverse(order)
	if sectionFiles[d.Name] {
		order = slices.Sorted(slices.Values(order)) // sectionKeep keeps each section's top bullets
	}
	kept := map[int]bool{}
	for _, i := range order {
		if sectionFiles[d.Name] && d.Entries[i].Kind == KindHeading || wanted[keys[i]] > 0 {
			wanted[keys[i]]--
			kept[i] = true
		}
	}
	return kept
}

// entryKeys is each entry's text without trailing blank lines, prefixed in section files by its section heading.
func entryKeys(d Doc) []string {
	keys, heading := make([]string, len(d.Entries)), ""
	for i, e := range d.Entries {
		if sectionFiles[d.Name] && e.Kind == KindHeading {
			heading = e.Lines[0]
		}
		keys[i] = heading + "\x00" + strings.TrimRight(e.block(), "\n")
	}
	return keys
}

// docOverflow uses the Doc's age keys, not the physical position, for oldest-first eviction.
func docOverflow(d Doc, capLines int) (header string, keep, overflow []string) {
	if logicalLineCount(string(d.Render())) <= capLines {
		return "", nil, nil
	}
	if sectionFiles[d.Name] {
		return sectionSplit(d.Name, d, capLines)
	}
	header, blocks := docBlocks(d, false)
	order := d.OldestFirst(0, len(d.Entries))
	slices.Reverse(order) // budgetSplit consumes newest first
	ordered := make([]string, 0, len(order))
	for _, i := range order {
		ordered = append(ordered, blocks[i])
	}
	keep, overflow = budgetSplit(header, ordered, capLines)
	if d.Grammar == GrammarBullets {
		slices.Reverse(keep)
		slices.Reverse(overflow)
	}
	return header, keep, overflow
}

// archiveFile prunes the typed file name under the bank lock, archiving the oldest overflow first so nothing is lost.
func archiveFile(name string, capLines int, now time.Time) (moved int, err error) {
	path := Path(name)
	err = withBankLock(BankLockWait, func() error {
		data, err := readFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		doc := normaliseSections(ParseDoc(name, data))
		_, keep, overflow := docOverflow(doc, capLines)
		if len(overflow) == 0 {
			if string(doc.Render()) != string(data) {
				if ok, err := writeIfStill(path, data, true, doc.Render(), 0o600); err != nil || !ok {
					return err
				}
				moved = 1 // report a normalization-only bank change to dream's changed probe
			}
			return nil
		}
		text, overflow := partitionDoc(doc, keep)
		if err := appendArchiveOverflow(name, now, overflow); err != nil {
			return err
		}
		if ok, err := writeIfStill(path, data, true, []byte(text), 0o600); err != nil || !ok {
			return err
		}
		moved = len(overflow)
		return nil
	})
	return moved, err
}

// appendArchiveOverflow appends only entries not already archived, including whitespace-variant retries.
func appendArchiveOverflow(name string, now time.Time, overflow []string) error {
	archive := archivePath(name, now)
	existing, err := readFile(archive)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, held := archivedBlocks(name, string(existing))
	heldSet := make(map[string]bool, len(held))
	for _, b := range held {
		heldSet[archiveKey(b)] = true
	}
	fresh := slices.DeleteFunc(slices.Clone(overflow), func(o string) bool { return heldSet[archiveKey(o)] })
	if len(fresh) > 0 {
		return appendBlocks(archive, fresh)
	}
	return nil
}

// PromoteTargets are the `memory feedback promote --to` destinations.
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
		return "", fmt.Errorf("--to claude-md only runs inside `memory update --dream`'s cross-pollination step "+
			"(gated: 3+ memory entries AND a hard prohibition) — route %s to rules instead: "+
			"`memory feedback promote %s --to rules`", id, id)
	}
	block, err := findBlock(id)
	if err != nil {
		return "", err
	}
	ref := assets.MemoryPromoteProcedure()
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

// findBlock searches typed files before their archive/<stem>-*.md overflow.
func findBlock(id string) (string, error) {
	for name := range idFiles {
		if blk, err := blockIn(Path(name), name, id); err != nil || blk != "" {
			return blk, err
		}
	}
	for name := range idFiles {
		ap := paths.MemorySub("archive", strings.TrimSuffix(name, ".md")+"-*.md")
		for _, p := range transcript.GlobIn(filepath.Dir(ap), filepath.Base(ap)) {
			blk, rerr := blockIn(p, name, id)
			if rerr != nil || blk != "" {
				return blk, rerr
			}
		}
	}
	return "", errNoBlock
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

// archiveKey ignores layout whitespace, but retains all words (including the overflow source).
func archiveKey(block string) string { return string(appendFields(nil, block)) }

// archivedBlocks parses an archive file: section overflow is stored as "## " blocks, the rest by its own grammar.
func archivedBlocks(name, body string) (string, []string) {
	if sectionFiles[name] {
		return splitHeadingBlocks(body)
	}
	return entryBlocks(name, body)
}

// sectionSplit keeps every section, sharing the cap evenly with rollover; repeats are merged before splitting.
func sectionSplit(base string, d Doc, capLines int) (header string, keep, overflow []string) {
	header, sections := docBlocks(d, true)
	left := capLines - logicalLineCount(header)
	for i, sec := range sections {
		head, rest, _ := strings.Cut(sec, "\n")
		intro, bullets := splitOn(rest, func(ln string) bool { return strings.HasPrefix(ln, "- ") })
		fixed := 2 + logicalLineCount(intro) // heading + the blank line rejoin adds
		kept, moved := sectionKeep(bullets, left/(len(sections)-i)-fixed)
		block := strings.Join(slices.DeleteFunc([]string{head, intro, strings.Join(kept, "\n")}, isBlank), "\n")
		keep = append(keep, block)
		left -= logicalLineCount(block) + 1
		if len(moved) > 0 {
			overflow = append(overflow, overflowBlock(base, head, strings.Join(moved, "\n")))
		}
	}
	return header, keep, overflow
}

// sectionKeep keeps bullets from the top while they fit budget lines; the first bullet always stays.
func sectionKeep(bullets []string, budget int) (keep, moved []string) {
	used := 0
	for i, b := range bullets {
		n := logicalLineCount(b)
		if i > 0 && (len(moved) > 0 || used+n > budget) {
			moved = append(moved, b)
			continue
		}
		keep = append(keep, b)
		used += n
	}
	return keep, moved
}

// overflowBlock is one archived "## " block naming the section and file it came from.
func overflowBlock(base, head, text string) string {
	return head + " (overflow from " + base + ")\n" + text
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// blockCost is a block's line cost including the blank separator rejoin inserts (none before block 0 sans header).
func blockCost(block string, i, used int) int {
	n := logicalLineCount(block)
	if used > 0 || i > 0 {
		n++
	}
	return n
}
