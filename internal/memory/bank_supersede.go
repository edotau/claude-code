package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

var (
	declarationDelimiters = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("`+|[\"“”‘’']")
	})
	// reviseRef finds declarations at a line or sentence boundary.
	reviseRef = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?im)(?:^|[.!?][ \t]+)[ \t]*(?:[-*+] +|[0-9]+[.)] +)?(supersedes\\s+(?:\"[^\"]*\"\\s+\\()?`?(z?[0-9a-f]{8}(?:-[0-9a-z]+)?)\\b`?\\)?)")
	})
	// replacedPointer is the line settle writes under an old entry's title.
	replacedPointer = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^Replaced by (z?[0-9a-f]{8}(?:-[0-9a-z]+)?)(?: \(\d{4}-\d{2}-\d{2}\))?\.$`)
	})
	// titlePrefix is what stays outside the strike: the heading or bullet mark and the date prefix.
	titlePrefix = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^(?:#+ |- )?(?:\d{4}-\d{2}-\d{2}\s*[—–-]\s*)?`)
	})
	// struckSpan and seeNote are what a title may carry beyond live text: the strike itself and a "(see <id>)" pointer.
	struckSpan = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`~~.*?~~`)
	})
	seeNote = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`\(see z?[0-9a-f]{8}(?:-[0-9a-z]+)?\)`)
	})
	// pointerLine is a whole line holding only the pointer settle writes.
	pointerLine = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^\s*Replaced by z?[0-9a-f]{8}(?:-[0-9a-z]+)?(?: \(\d{4}-\d{2}-\d{2}\))?\.$`)
	})
)

// reviseFiles are the typed files whose entries replace one another (archives of them included).
var reviseFiles = []string{"decisionLog.md", "conventions.md"}

// reviseHopCap bounds how far recall follows a chain of replacements.
const reviseHopCap = 8

// bankDoc is one typed bank file or archive split into blocks, for edits that span entries.
type bankDoc struct {
	path   string
	header string
	text   string // current file text, rebuilt from the header and blocks on edit
	blocks []string
	ids    []string   // ids[i] is blocks[i]'s persisted id, "" when it has none
	refs   [][]string // refs[i] are the ids blocks[i] says it supersedes
	dirty  bool
}

// setBlock replaces block i, in both the block list and the file text, and flags the doc dirty.
func (d *bankDoc) setBlock(i int, block string) {
	d.blocks[i] = block
	d.text = rejoin(d.header, d.blocks)
	d.dirty = true
}

// blockLoc addresses one block: its doc and index.
type blockLoc struct{ doc, idx int }

// revision is one planned marking: the old block gets struck and points at the new entry.
type revision struct {
	old     blockLoc
	newID   string
	newDate string
}

// revisedIDs lists the ids block says it supersedes, in order, without repeats or its own id.
func revisedIDs(block, self string) []string {
	if !strings.Contains(strings.ToLower(block), "supersedes") {
		return nil
	}
	_, body, _ := strings.Cut(block, "\n")
	lines := strings.Split(body, "\n")
	fenced := fencedLines(lines)
	for i, line := range lines {
		if fenced[i] || strings.HasPrefix(strings.TrimSpace(line), ">") {
			lines[i] = ""
		}
	}
	body = strings.Join(lines, "\n")
	var out []string
	for _, m := range reviseRef().FindAllStringSubmatchIndex(body, -1) {
		id := body[m[4]:m[5]]
		if declarationOutside(body[:m[2]]) && id != self && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// declarationOutside rejects declarations inside inline code or quoted examples.
func declarationOutside(prefix string) bool {
	quote, ticks := "", ""
	for _, at := range declarationDelimiters().FindAllStringIndex(prefix, -1) {
		token := prefix[at[0]:at[1]]
		if strings.HasPrefix(token, "`") {
			ticks = toggleDelimiter(ticks, token)
			continue
		}
		if ticks == "" {
			quote = declarationQuote(quote, token, prefix[:at[0]])
		}
	}
	return quote == "" && ticks == ""
}

func toggleDelimiter(open, token string) string {
	if open == token {
		return ""
	}
	if open == "" {
		return token
	}
	return open
}

func declarationQuote(open, token, prefix string) string {
	if open != "" {
		if open == token {
			return ""
		}
		return open
	}
	switch token {
	case "“":
		return "”"
	case "‘":
		return "’"
	case "\"":
		return token
	case "'":
		if prefix == "" || strings.ContainsAny(prefix[len(prefix)-1:], " \t\n(") {
			return token
		}
	}
	return ""
}

// replacedBy accepts only the pointer immediately below the title, including wrapped bullet continuations.
func replacedBy(block string) string {
	if !strings.Contains(block, "Replaced") {
		return ""
	}
	title, body := replacementBody(block)
	if partlyStruck(title) {
		return ""
	}
	pointer := ""
	for line := range strings.SplitSeq(body, "\n") {
		if pointer != "" {
			pointer += " "
		}
		pointer += strings.TrimSpace(line)
		if m := replacedPointer().FindStringSubmatch(pointer); m != nil {
			return m[1]
		}
		if !strings.HasPrefix("Replaced by ", pointer) && !strings.HasPrefix(pointer, "Replaced by ") {
			break
		}
		if strings.ContainsAny(pointer, "\"`>") {
			break
		}
	}
	return ""
}

// replacementBody skips the wrapped title of a conventions bullet, through its id marker.
func replacementBody(block string) (title, body string) {
	title, body, _ = strings.Cut(block, "\n")
	if !strings.HasPrefix(title, "- ") || idMarker().MatchString(title) {
		return title, body
	}
	parts := []string{title}
	for strings.HasPrefix(body, "  ") {
		line, rest, _ := strings.Cut(body, "\n")
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "Replaced") || trimmed == "" || nextFence("", trimmed) != "" {
			break
		}
		parts = append(parts, strings.TrimSpace(line))
		body = rest
		if idMarker().MatchString(line) {
			break
		}
	}
	return strings.Join(parts, " "), body
}

// partlyStruck reports a title with a ~~strike~~ and live text beside it: two decisions, only one reversed.
func partlyStruck(first string) bool {
	if strings.Count(first, "~~") < 2 {
		return false
	}
	body := stripID(first)
	body = body[len(titlePrefix().FindString(body)):]
	rest := seeNote().ReplaceAllString(struckSpan().ReplaceAllString(body, ""), "")
	return strings.Trim(rest, " \t;,.:—–-") != ""
}

// isSupersededBlock reports a struck-out or SUPERSEDED title or a `Replaced by` pointer; a partly struck one is live.
func isSupersededBlock(block string) bool {
	return isSupersededTitle(textutil.FirstLine(block)) || replacedBy(block) != ""
}

// dropPointer is block without the lines that hold only a `Replaced by` pointer.
func dropPointer(block string) string {
	lines := strings.Split(block, "\n")
	return strings.Join(slices.DeleteFunc(lines, pointerLine().MatchString), "\n")
}

// healPartial removes the pointer settle once wrote under a partly struck entry, so recall serves it again.
func healPartial(docs []*bankDoc) {
	for _, d := range docs {
		for i, b := range d.blocks {
			if strings.Contains(b, "Replaced by") && partlyStruck(textutil.FirstLine(b)) {
				if healed := dropPointer(b); healed != b {
					d.setBlock(i, healed)
				}
			}
		}
	}
}

// newBankDoc splits text (the content of path) into blocks with their ids and the ids each supersedes.
func newBankDoc(path, text string) *bankDoc {
	header, blocks := entryBlocks(logicalName(path), text)
	d := &bankDoc{path: path, header: header, text: text, blocks: blocks, ids: make([]string, len(blocks)), refs: make([][]string, len(blocks))}
	for i, b := range blocks {
		d.ids[i] = ownerID(b)
		d.refs[i] = revisedIDs(b, d.ids[i])
	}
	return d
}

// readDoc reads and splits one bank file; nil for an absent one.
func readDoc(path string) (*bankDoc, error) {
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return newBankDoc(path, string(data)), nil
}

// reviseFilesOf lists the live typed files, then (archives true) their archive files.
func reviseFilesOf(archives bool) []string {
	var out []string
	for _, name := range reviseFiles {
		out = append(out, Path(name))
	}
	if !archives {
		return out
	}
	for _, name := range reviseFiles {
		p := paths.MemorySub("archive", "*"+strings.TrimSuffix(name, ".md")+"-*.md") // * keeps pre-merge <key>-- archives
		out = append(out, transcript.GlobIn(filepath.Dir(p), filepath.Base(p))...)
	}
	return out
}

// missingRefs are the marker strings of ids some doc supersedes that no doc holds.
func missingRefs(docs []*bankDoc) []string {
	loc := locateBlocks(docs)
	var out []string
	for _, d := range docs {
		for _, refs := range d.refs {
			for _, id := range refs {
				if _, ok := loc[id]; !ok && !slices.Contains(out, "<!-- id:"+id+" -->") {
					out = append(out, "<!-- id:"+id+" -->")
				}
			}
		}
	}
	return out
}

// liveDocs is the bank's live typed files, from texts when the settle already holds them, else read.
func liveDocs(texts map[string]string) ([]*bankDoc, error) {
	var docs []*bankDoc
	for _, name := range reviseFiles {
		p := Path(name)
		text, ok := texts[name]
		if !ok {
			d, err := readDoc(p)
			if err != nil {
				return nil, err
			}
			if d != nil {
				docs = append(docs, d)
			}
			continue
		}
		docs = append(docs, newBankDoc(p, text))
	}
	return docs, nil
}

// holderDocs are the files among paths whose text carries any marker; an absent file is skipped.
func holderDocs(paths, markers []string) ([]*bankDoc, error) {
	var docs []*bankDoc
	for _, p := range paths {
		data, err := readFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(markers, func(m string) bool { return strings.Contains(string(data), m) }) {
			docs = append(docs, newBankDoc(p, string(data)))
		}
	}
	return docs, nil
}

// reviseDocSet is liveDocs plus, only when a reference names an id they lack, the other files carrying it.
func reviseDocSet(texts map[string]string) ([]*bankDoc, error) {
	docs, err := liveDocs(texts)
	if err != nil {
		return nil, err
	}
	missing := missingRefs(docs)
	if len(missing) == 0 {
		return docs, nil
	}
	own := reviseFilesOf(false)
	rest := slices.DeleteFunc(reviseFilesOf(true), func(p string) bool { return slices.Contains(own, p) })
	for len(missing) > 0 {
		extra, err := holderDocs(rest, missing)
		if err != nil || len(extra) == 0 {
			return docs, err
		}
		docs = append(docs, extra...)
		rest = slices.DeleteFunc(rest, func(p string) bool {
			return slices.ContainsFunc(extra, func(d *bankDoc) bool { return d.path == p })
		})
		missing = missingRefs(docs)
	}
	return docs, nil
}

// statOK reports whether path exists.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// slugMarker is a title-line alias id such as "<!-- id:b5ad9e07-semorder -->" that entries may be superseded by.
var slugMarker = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`<!-- id:([0-9a-f]{8}-[0-9a-z]+) -->`)
})

// locateBlocks maps each persisted block id, and each title-line slug alias, to its first location.
func locateBlocks(docs []*bankDoc) map[string]blockLoc {
	loc := map[string]blockLoc{}
	for di, d := range docs {
		for bi, id := range d.ids {
			if _, dup := loc[id]; id != "" && !dup {
				loc[id] = blockLoc{di, bi}
			}
			for _, m := range slugMarker().FindAllStringSubmatch(textutil.FirstLine(d.blocks[bi]), -1) {
				if _, dup := loc[m[1]]; !dup {
					loc[m[1]] = blockLoc{di, bi}
				}
			}
		}
	}
	return loc
}

// reviseEdges maps each entry id to the ids it says it supersedes.
func reviseEdges(docs []*bankDoc) map[string][]string {
	edges := map[string][]string{}
	for _, d := range docs {
		for i, id := range d.ids {
			if id != "" && len(d.refs[i]) > 0 {
				edges[id] = append(edges[id], d.refs[i]...)
			}
		}
	}
	return edges
}

// reaches reports whether following supersede edges from `from` arrives at `to`.
func reaches(edges map[string][]string, from, to string) bool {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == to {
			return true
		}
		if !seen[id] {
			seen[id] = true
			stack = append(stack, edges[id]...)
		}
	}
	return false
}

// blockDate is the YYYY-MM-DD in block's title line, "" when undated.
func blockDate(block string) string {
	return entryDate().FindString(stripID(textutil.FirstLine(block)))
}

// revisePlan is the lookup state planRevisions classifies each reference against.
type revisePlan struct {
	docs    []*bankDoc
	loc     map[string]blockLoc
	edges   map[string][]string
	claimed map[string]bool
}

// pair classifies "newID supersedes oldID": a revision to apply, a problem to report, or neither.
func (p *revisePlan) pair(newID, oldID, newDate string) (*revision, string) {
	ol, known := p.loc[oldID]
	switch {
	case !known:
		return nil, fmt.Sprintf("supersedes: %s names unknown id %s", newID, oldID)
	case reaches(p.edges, oldID, newID):
		if newID < oldID { // one line per cycle
			return nil, fmt.Sprintf("supersedes: %s and %s name each other (neither marked)", newID, oldID)
		}
	case partlyStruck(textutil.FirstLine(p.docs[ol.doc].blocks[ol.idx])): // half of it is still valid: never replaced
	case replacedBy(p.docs[ol.doc].blocks[ol.idx]) == "" && !p.claimed[oldID]:
		p.claimed[oldID] = true
		return &revision{old: ol, newID: newID, newDate: newDate}, ""
	}
	return nil, ""
}

// planRevisions turns every `Supersedes <id>` in docs into a revision; dangling and cyclic ones become problems.
func planRevisions(docs []*bankDoc) (revs []revision, problems []string) {
	p := &revisePlan{docs: docs, loc: locateBlocks(docs), edges: reviseEdges(docs), claimed: map[string]bool{}}
	for _, d := range docs {
		for i, newID := range d.ids {
			for _, oldID := range d.refs[i] {
				rev, problem := p.pair(newID, oldID, blockDate(d.blocks[i]))
				if rev != nil {
					revs = append(revs, *rev)
				}
				if problem != "" {
					problems = append(problems, problem)
				}
			}
		}
	}
	return revs, problems
}

// strikeTitle strikes the title text of a block's first line, keeping the heading/bullet, date prefix and id marker.
func strikeTitle(line string) string {
	if isSupersededTitle(line) {
		return line
	}
	body, tail := line, ""
	if at := idMarker().FindStringIndex(line); at != nil {
		body, tail = line[:at[0]], line[at[0]:]
	}
	prefix := titlePrefix().FindString(body)
	text := strings.TrimRight(body[len(prefix):], " \t")
	if text == "" {
		return line
	}
	return prefix + "~~" + text + "~~" + tail
}

// markReplaced strikes block's title and adds the pointer line under it.
func markReplaced(block, newID, newDate string) string {
	ptr := "Replaced by " + newID
	if newDate != "" {
		ptr += " (" + newDate + ")"
	}
	ptr += "."
	lines := strings.Split(block, "\n")
	lines[0] = strikeTitle(lines[0])
	if strings.HasPrefix(lines[0], "- ") { // a bullet's continuation lines are indented
		ptr = "  " + ptr
	}
	return strings.Join(slices.Insert(lines, 1, ptr), "\n")
}

// applyRevisions marks each revision's old block in place and flags its doc dirty.
func applyRevisions(docs []*bankDoc, revs []revision) {
	for _, r := range revs {
		d := docs[r.old.doc]
		d.setBlock(r.old.idx, markReplaced(d.blocks[r.old.idx], r.newID, r.newDate))
	}
}

// writeDirtyDocs rewrites the docs a revision touched.
func writeDirtyDocs(docs []*bankDoc) error {
	for _, d := range docs {
		if d.dirty {
			d.text = formatBankMarkdown(d.text) // a strike or pointer may push a line past the wrap width
			if err := paths.AtomicWrite(d.path, []byte(d.text), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

// reviseDir applies every `Supersedes <id>` in the bank as a strike + pointer; texts are the settle's loaded files.
func reviseDir(texts map[string]string) error {
	docs, err := reviseDocSet(texts)
	if err != nil {
		return err
	}
	healPartial(docs)
	revs, _ := planRevisions(docs)
	applyRevisions(docs, revs)
	return writeDirtyDocs(docs)
}

// reviseBankProblems lists dangling and cyclic `Supersedes` references in the bank, for `memory review`.
func reviseBankProblems() []string {
	docs, err := reviseDocSet(nil)
	if err != nil {
		return nil
	}
	var out []string
	_, problems := planRevisions(docs)
	for _, p := range problems {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// chainEnd follows h's `Replaced by` pointers to the live entry, within reviseHopCap hops; false when it breaks.
func chainEnd(h SearchHit, find func(string) (SearchHit, bool)) (SearchHit, bool) {
	cur := h
	for hop := 0; hop <= reviseHopCap; hop++ {
		next := replacedBy(cur.Block)
		if next == "" {
			return cur, !isSupersededBlock(cur.Block)
		}
		if hop == reviseHopCap {
			break
		}
		n, ok := find(next)
		if !ok {
			return SearchHit{}, false
		}
		cur = n
	}
	return SearchHit{}, false
}

// blockFinder returns the lookup of a corpus block by persisted id; the first block per id wins.
func blockFinder(files []corpusFile, segs []segView) func(id string) (SearchHit, bool) {
	return func(id string) (SearchHit, bool) {
		needle := []byte("<!-- id:" + id + " -->")
		for i, seg := range segs {
			if seg == nil { // a missing or unreadable file has no segment
				continue
			}
			for d := range seg.Docs() {
				raw, _ := seg.docRaw(d)
				if !bytes.Contains(raw, needle) {
					continue
				}
				block := string(raw)
				if ownerID(block) != id {
					continue
				}
				f := files[i]
				return SearchHit{Bank: f.Bank, File: filepath.Base(f.Path), Block: block, ID: id, Dir: accessDir()}, true
			}
		}
		return SearchHit{}, false
	}
}

// followReplaced drops struck or replaced hits; each replaced one hands its score to the live end of its chain, once.
func followReplaced(ranked []SearchHit, files []corpusFile, segs []segView) []SearchHit {
	if !slices.ContainsFunc(ranked, SearchHit.isSuperseded) {
		return ranked // the common case: nothing replaced, nothing allocated
	}
	out := make([]SearchHit, 0, len(ranked))
	at := map[string]int{} // blockKey -> index in out
	followed := false
	find := blockFinder(files, segs)
	add := func(h SearchHit) {
		key := h.key()
		if i, dup := at[key]; dup {
			if h.Score > out[i].Score {
				out[i].Score, out[i].Matched = h.Score, h.Matched
			}
			return
		}
		at[key] = len(out)
		out = append(out, h)
	}
	for _, h := range ranked {
		if !h.isSuperseded() {
			add(h)
		} else if replacedBy(h.Block) != "" {
			if live, ok := chainEnd(h, find); ok {
				add(live.withScore(h))
				followed = true
			} else {
				h.UnresolvedReplacement = true
				h.Block += "\n[Replacement unavailable; showing the original entry.]"
				h.sum = 0
				add(h)
			}
		}
	}
	if followed {
		out = sortByScore(out)
	}
	return out
}

// withScore is the hit carrying from's score and matched-term count.
func (h SearchHit) withScore(from SearchHit) SearchHit {
	h.Score, h.Matched = from.Score, from.Matched
	return h
}
