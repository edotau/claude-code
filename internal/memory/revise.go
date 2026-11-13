package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

var (
	// reviseRef finds declarations at a line or sentence boundary.
	declarationDelimiters = regexp.MustCompile("`+|[\"“”‘’']")
	reviseRef             = regexp.MustCompile("(?im)(?:^|[.!?][ \t]+)[ \t]*(?:[-*+] +|[0-9]+[.)] +)?(supersedes\\s+(?:\"[^\"]*\"\\s+\\()?`?(z?[0-9a-f]{8})\\b`?\\)?)")
	// replacedPointer is the line settle writes under an old entry's title.
	replacedPointer = regexp.MustCompile(`^Replaced by (z?[0-9a-f]{8})(?: \(\d{4}-\d{2}-\d{2}\))?\.$`)
	// titlePrefix is what stays outside the strike: the heading or bullet mark and the date prefix.
	titlePrefix = regexp.MustCompile(`^(?:#+ |- )?(?:\d{4}-\d{2}-\d{2}\s*[—–-]\s*)?`)
	// struckSpan and seeNote are what a title may carry beyond live text: the strike itself and a "(see <id>)" pointer.
	struckSpan = regexp.MustCompile(`~~.*?~~`)
	seeNote    = regexp.MustCompile(`\(see z?[0-9a-f]{8}\)`)
	// pointerLine is a whole line holding only the pointer settle writes.
	pointerLine = regexp.MustCompile(`^\s*Replaced by z?[0-9a-f]{8}(?: \(\d{4}-\d{2}-\d{2}\))?\.$`)
)

// reviseFiles are the typed files whose entries replace one another (archives of them included).
var reviseFiles = []string{"decisionLog.md", "conventions.md"}

// reviseHopCap bounds how far recall follows a chain of replacements.
const reviseHopCap = 8

// ErrReviseInput marks a revise request the caller got wrong (unknown, equal or cyclic ids); the CLI exits 2.
var ErrReviseInput = errors.New("memory revise")

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
	old          blockLoc
	oldID, newID string
	newDate      string
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
	for _, m := range reviseRef.FindAllStringSubmatchIndex(body, -1) {
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
	for _, at := range declarationDelimiters.FindAllStringIndex(prefix, -1) {
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
		if m := replacedPointer.FindStringSubmatch(pointer); m != nil {
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
	if !strings.HasPrefix(title, "- ") || idMarker.MatchString(title) {
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
		if idMarker.MatchString(line) {
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
	body = body[len(titlePrefix.FindString(body)):]
	rest := seeNote.ReplaceAllString(struckSpan.ReplaceAllString(body, ""), "")
	return strings.Trim(rest, " \t;,.:—–-") != ""
}

// isSupersededBlock reports a struck-out or SUPERSEDED title or a `Replaced by` pointer; a partly struck one is live.
func isSupersededBlock(block string) bool {
	return isSupersededTitle(textutil.FirstLine(block)) || replacedBy(block) != ""
}

// dropPointer is block without the lines that hold only a `Replaced by` pointer.
func dropPointer(block string) string {
	lines := strings.Split(block, "\n")
	return strings.Join(slices.DeleteFunc(lines, pointerLine.MatchString), "\n")
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

// reviseFilesOf lists the live typed files of each dir, then (archives true) their archive files.
func reviseFilesOf(dirs []string, archives bool) []string {
	var out []string
	for _, dir := range dirs {
		for _, name := range reviseFiles {
			out = append(out, filepath.Join(dir, name))
		}
	}
	if !archives {
		return out
	}
	for _, dir := range dirs {
		for _, name := range reviseFiles {
			out = append(out, globIn(archiveDir(dir), strings.TrimSuffix(name, ".md")+"-*.md")...)
		}
	}
	return out
}

// loadBankDocs reads every supersedable file of dirs, archives included, for the revise verb.
func loadBankDocs(dirs ...string) ([]*bankDoc, error) {
	var docs []*bankDoc
	for _, p := range reviseFilesOf(dirs, true) {
		d, err := readDoc(p)
		if err != nil {
			return nil, err
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs, nil
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

// liveDocs is dir's live typed files, from texts when the settle already holds them, else read.
func liveDocs(dir string, texts map[string]string) ([]*bankDoc, error) {
	var docs []*bankDoc
	for _, name := range reviseFiles {
		p := filepath.Join(dir, name)
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
func reviseDocSet(dir string, texts map[string]string) ([]*bankDoc, error) {
	docs, err := liveDocs(dir, texts)
	if err != nil {
		return nil, err
	}
	missing := missingRefs(docs)
	if len(missing) == 0 {
		return docs, nil
	}
	others := []string{dir}
	if g := HomeBankDir(); !paths.SamePath(dir, g) {
		others = append(others, g)
	}
	own := reviseFilesOf([]string{dir}, false)
	rest := slices.DeleteFunc(reviseFilesOf(others, true), func(p string) bool { return slices.Contains(own, p) })
	extra, err := holderDocs(rest, missing)
	return append(docs, extra...), err
}

// bankDirs is the stores a settle covers: root's own bank when scaffolded, then the home bank.
func bankDirs(root string) []string {
	var dirs []string
	if own := Dir(root); statOK(own) {
		dirs = append(dirs, own)
	}
	if g := HomeBankDir(); len(dirs) == 0 || !paths.SamePath(dirs[0], g) {
		dirs = append(dirs, g)
	}
	return dirs
}

// statOK reports whether path exists.
func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// locateBlocks maps each persisted block id to its first location.
func locateBlocks(docs []*bankDoc) map[string]blockLoc {
	loc := map[string]blockLoc{}
	for di, d := range docs {
		for bi, id := range d.ids {
			if _, dup := loc[id]; id != "" && !dup {
				loc[id] = blockLoc{di, bi}
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
	return entryDate.FindString(stripID(textutil.FirstLine(block)))
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
	case !revisionVisible(p.docs[ol.doc].path, p.docs[p.loc[newID].doc].path):
		return nil, fmt.Sprintf("supersedes: %s: a home entry may be replaced only by a home entry", newID)
	case reaches(p.edges, oldID, newID):
		if newID < oldID { // one line per cycle
			return nil, fmt.Sprintf("supersedes: %s and %s name each other (neither marked)", newID, oldID)
		}
	case partlyStruck(textutil.FirstLine(p.docs[ol.doc].blocks[ol.idx])): // half of it is still valid: never replaced
	case replacedBy(p.docs[ol.doc].blocks[ol.idx]) == "" && !p.claimed[oldID]:
		p.claimed[oldID] = true
		return &revision{old: ol, oldID: oldID, newID: newID, newDate: newDate}, ""
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
	if at := idMarker.FindStringIndex(line); at != nil {
		body, tail = line[:at[0]], line[at[0]:]
	}
	prefix := titlePrefix.FindString(body)
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

// reviseDir applies every `Supersedes <id>` found in dir as a strike + pointer; texts are the settle's loaded files.
func reviseDir(dir string, texts map[string]string) error {
	docs, err := reviseDocSet(dir, texts)
	if err != nil {
		return err
	}
	healPartial(docs)
	revs, _ := planRevisions(docs)
	applyRevisions(docs, revs)
	return writeDirtyDocs(docs)
}

// reviseProblems lists dangling and cyclic `Supersedes` references in root's banks, for `memory review`.
func reviseProblems(root string) []string {
	var out []string
	for _, dir := range bankDirs(root) {
		docs, err := reviseDocSet(dir, nil)
		if err != nil {
			continue
		}
		_, problems := planRevisions(docs)
		for _, p := range problems {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// ReviseEntry makes newID replace oldID: a `Supersedes` line in the new entry, a strike + pointer on the old one.
func ReviseEntry(root, oldID, newID string, dryRun bool) (string, error) {
	if oldID == newID {
		return "", fmt.Errorf("%w: old and new id are both %s", ErrReviseInput, oldID)
	}
	var msg string
	err := withReviseLocks(root, func() error {
		if skipped, err := recoverEach(); err != nil || skipped != nil {
			return errors.Join(err, skipped)
		}
		docs, err := loadBankDocs(bankDirs(root)...)
		if err != nil {
			return err
		}
		msg, err = reviseDocs(docs, oldID, newID, dryRun)
		return err
	})
	return msg, err
}

// withReviseLocks takes store, home and bank locks in the worker order, with the foreground wait.
func withReviseLocks(root string, fn func() error) error {
	dirs := []string{Dir(root)}
	if !paths.SamePath(dirs[0], HomeBankDir()) {
		dirs = append(dirs, HomeBankDir())
	}
	var lock func(int) error
	lock = func(i int) error {
		if i == len(dirs) {
			return WithBankLock(BankLockWait, fn)
		}
		ran, err := paths.TryFileLock(lockPath(dirs[i])+".lock", updateLockWait, func() error { return lock(i + 1) })
		if err == nil && !ran {
			return errors.New("memory revise: busy: another update or dream holds the bank; retry when it finishes")
		}
		return err
	}
	return lock(0)
}

// checkRevise refuses unknown ids and cycles, and returns the "nothing to do" note when oldID is already replaced.
func checkRevise(docs []*bankDoc, loc map[string]blockLoc, oldID, newID string) (note string, err error) {
	for _, id := range []string{oldID, newID} {
		if _, ok := loc[id]; !ok {
			return "", fmt.Errorf("%w: unknown id %s", ErrReviseInput, id)
		}
	}
	ol := loc[oldID]
	if !revisionVisible(docs[ol.doc].path, docs[loc[newID].doc].path) {
		return "", fmt.Errorf("%w: a home entry may be replaced only by a home entry", ErrReviseInput)
	}
	if partlyStruck(textutil.FirstLine(docs[ol.doc].blocks[ol.idx])) {
		return "", fmt.Errorf("%w: %s's title is only partly struck, so half of it is still valid; split the entry first", ErrReviseInput, oldID)
	}
	if by := replacedBy(docs[ol.doc].blocks[ol.idx]); by != "" {
		return fmt.Sprintf("nothing to do: %s is already replaced by %s\n", oldID, by), nil
	}
	if reaches(reviseEdges(docs), oldID, newID) {
		return "", fmt.Errorf("%w: %s already supersedes %s", ErrReviseInput, oldID, newID)
	}
	return "", nil
}

// revisionVisible preserves visibility wherever the old entry can be recalled.
func revisionVisible(oldPath, newPath string) bool {
	home := HomeBankDir() + string(filepath.Separator)
	return !strings.HasPrefix(oldPath, home) || strings.HasPrefix(newPath, home)
}

// reviseDocs plans the revision of oldID by newID over docs and commits it unless dryRun.
func reviseDocs(docs []*bankDoc, oldID, newID string, dryRun bool) (string, error) {
	loc := locateBlocks(docs)
	if note, err := checkRevise(docs, loc, oldID, newID); err != nil || note != "" {
		return note, err
	}
	ol, nl := loc[oldID], loc[newID]
	nd := docs[nl.doc]
	if !slices.Contains(nd.refs[nl.idx], oldID) {
		nd.setBlock(nl.idx, nd.blocks[nl.idx]+"\nSupersedes "+oldID+".")
	}
	applyRevisions(docs, []revision{{old: ol, oldID: oldID, newID: newID, newDate: blockDate(nd.blocks[nl.idx])}})
	changes, err := dirtyChanges(docs)
	if err != nil {
		return "", err
	}
	rels := make([]string, len(changes))
	for i, c := range changes {
		rels[i] = "  " + c.Rel + "\n"
	}
	head := fmt.Sprintf("%s is replaced by %s in:\n%s", oldID, newID, strings.Join(rels, ""))
	if dryRun {
		return "dry run, nothing written: " + head, nil
	}
	rec, err := commitChangesLocked("memory: revise "+oldID+" replaced by "+newID, changes)
	if err == nil && len(rec.Divergent) > 0 {
		err = fmt.Errorf("%w: files changed underneath: %s", errDivergent, strings.Join(rec.Divergent, ", "))
	}
	return head, err
}

// dirtyChanges is the store-relative rewrite of every doc a revision touched.
func dirtyChanges(docs []*bankDoc) ([]Change, error) {
	var changes []Change
	for _, d := range docs {
		if !d.dirty {
			continue
		}
		rel, err := filepath.Rel(StoreDir(), d.path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, Change{Rel: filepath.ToSlash(rel), Data: []byte(d.text)})
	}
	return changes, nil
}

// chainEnd follows h's `Replaced by` pointers to the live entry, within reviseHopCap hops; false when it breaks.
func chainEnd(h SearchHit, files []corpusFile, segs []segView) (SearchHit, bool) {
	cur := h
	for hop := 0; hop <= reviseHopCap; hop++ {
		next := replacedBy(cur.Block)
		if next == "" {
			return cur, !isSupersededBlock(cur.Block)
		}
		if hop == reviseHopCap {
			break
		}
		n, ok := findBlockByID(next, files, segs)
		if !ok {
			return SearchHit{}, false
		}
		cur = n
	}
	return SearchHit{}, false
}

// findBlockByID is the corpus block carrying persisted id, scanning the segments the search already holds.
func findBlockByID(id string, files []corpusFile, segs []segView) (SearchHit, bool) {
	marker := "<!-- id:" + id + " -->"
	for i, seg := range segs {
		if seg == nil { // a missing or unreadable file has no segment
			continue
		}
		for d := range seg.Docs() {
			if block, _ := seg.Doc(d); strings.Contains(block, marker) && ownerID(block) == id {
				f := files[i]
				return SearchHit{Bank: f.Bank, File: filepath.Base(f.Path), Block: block, ID: id, Dir: ownerDir(f.Bank, f.Path)}, true
			}
		}
	}
	return SearchHit{}, false
}

// followReplaced drops struck or replaced hits; each replaced one hands its score to the live end of its chain, once.
func followReplaced(ranked []SearchHit, files []corpusFile, segs []segView) []SearchHit {
	if !slices.ContainsFunc(ranked, func(h SearchHit) bool { return isSupersededBlock(h.Block) }) {
		return ranked // the common case: nothing replaced, nothing allocated
	}
	out := make([]SearchHit, 0, len(ranked))
	at := map[string]int{} // blockKey -> index in out
	followed := false
	add := func(h SearchHit) {
		if i, dup := at[blockKey(h.Block)]; dup {
			if h.Score > out[i].Score {
				out[i].Score, out[i].Matched = h.Score, h.Matched
			}
			return
		}
		at[blockKey(h.Block)] = len(out)
		out = append(out, h)
	}
	for _, h := range ranked {
		if !isSupersededBlock(h.Block) {
			add(h)
		} else if replacedBy(h.Block) != "" {
			if live, ok := chainEnd(h, files, segs); ok {
				add(live.withScore(h))
				followed = true
			} else {
				h.UnresolvedReplacement = true
				h.Block += "\n[Replacement unavailable; showing the original entry.]"
				add(h)
			}
		}
	}
	if followed {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	}
	return out
}

// withScore is the hit carrying from's score and matched-term count.
func (h SearchHit) withScore(from SearchHit) SearchHit {
	h.Score, h.Matched = from.Score, from.Matched
	return h
}

// RecallRepo is SearchRepo as recall serves it: replaced entries give way to their live replacement, top k kept.
func RecallRepo(repo string, all bool, q string, k int) ([]SearchHit, string, error) {
	if all {
		files := allCorpusFiles()
		segs := loadSegments(files, false)
		hits := followReplaced(rankFiles(contentTerms(q), 0, files, segs), files, segs)
		if k > 0 && len(hits) > k {
			hits = hits[:k]
		}
		return hits, root(repo), nil
	}
	top, err := ResolveRepo(repo)
	if err != nil {
		return nil, "", err
	}
	ranked, files, segs := searchBanksTerms(top, q, contentTerms(q), 0)
	hits := followReplaced(ranked, files, segs)
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, top, nil
}
