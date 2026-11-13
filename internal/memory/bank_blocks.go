package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// readFile is os.ReadFile for typed bank files; a var so tests can count how often one file is read.
var readFile = os.ReadFile

// indexLineMax keeps routing lines scannable; detail lives in the entry itself.
const indexLineMax = 120

// entryBlocks splits by the grammar the file NAME implies: dated logs on "## " only, others also on "- ".
func entryBlocks(base, body string) (header string, blocks []string) {
	if base == "decisionLog.md" || base == "sessionHistory.md" {
		return splitHeadingBlocks(body)
	}
	return splitOn(body, func(ln string) bool {
		return strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "## ")
	})
}

// splitHeadingBlocks splits a newest-first log into its header and "## " entries (bodies may hold bullets).
func splitHeadingBlocks(body string) (header string, blocks []string) {
	return splitOn(body, func(ln string) bool { return strings.HasPrefix(ln, "## ") })
}

// nextFence returns the open fence's marker run after ln: an opener sets it, only a bare run of its character closes.
func nextFence(open, ln string) string {
	t := strings.TrimSpace(ln)
	if open != "" {
		if fenceCloses(t, open) {
			return ""
		}
		return open
	}
	if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		return fenceRun(t)
	}
	return ""
}

// fenceRun is the leading run of fence characters in a trimmed fence line ("```" for "```go").
func fenceRun(trimmed string) string {
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	return trimmed[:n]
}

// fenceCloses reports whether trimmed is a bare run of open's character at least as long as open.
func fenceCloses(trimmed, open string) bool {
	return len(trimmed) >= len(open) && strings.Trim(trimmed, open[:1]) == ""
}

// splitOn is the fence-aware splitter: a marker-shaped line inside a fence never starts a block.
func splitOn(body string, isMarker func(string) bool) (header string, blocks []string) {
	var hdr, cur []string
	inBlock := false
	fence := "" // marker run of the open fence, "" outside one
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.TrimRight(strings.Join(cur, "\n"), "\n"))
			cur = nil
		}
	}
	for ln := range strings.SplitSeq(body, "\n") {
		if fence == "" && isMarker(ln) {
			flush()
			inBlock = true
		}
		fence = nextFence(fence, ln)
		if inBlock {
			cur = append(cur, ln)
		} else {
			hdr = append(hdr, ln)
		}
	}
	flush()
	return strings.TrimRight(strings.Join(hdr, "\n"), "\n"), blocks
}

// rejoin reassembles a bank file from its header and kept blocks.
func rejoin(header string, blocks []string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n")
	}
	for _, blk := range blocks {
		b.WriteString("\n")
		b.WriteString(blk)
		b.WriteString("\n")
	}
	out := strings.TrimLeft(b.String(), "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// appendBlocks appends blocks to path, creating it with a minimal header when absent.
func appendBlocks(path string, blocks []string) error {
	existing, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // never overwrite an archive we could not read
	}
	return paths.AtomicWrite(path, []byte(appendedText(path, string(existing), blocks)), 0o600)
}

// appendedText is body with blocks appended, under a minimal header when body is blank.
func appendedText(path, body string, blocks []string) string {
	if strings.TrimSpace(body) == "" {
		body = "# " + strings.TrimSuffix(filepath.Base(path), ".md") + " (archived)\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	var b strings.Builder
	b.WriteString(body)
	for _, block := range blocks {
		b.WriteByte('\n')
		b.WriteString(block)
		b.WriteByte('\n')
	}
	return b.String()
}

// entriesWithPrefix returns every line starting with prefix, prefix stripped and truncated, in file order.
func entriesWithPrefix(path, prefix string) ([]string, error) {
	data, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	var out []string
	lines := strings.Split(string(data), "\n")
	fence := "" // marker run of the open fence; a prefixed line inside one is not an entry
	for i, line := range lines {
		inFence := fence != ""
		fence = nextFence(fence, line)
		if inFence {
			continue
		}
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			e := truncateLine(stripID(rest))
			if id := ownerID(wrappedBlock(lines, i)); id != "" {
				e += " [" + id + "]"
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// wrappedBlock is lines[i] and the indented continuation lines the formatter wrapped under it, joined.
func wrappedBlock(lines []string, i int) string {
	end := i + 1
	for end < len(lines) && strings.HasPrefix(lines[end], " ") && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	return strings.Join(lines[i:end], "\n")
}

// truncateLine caps s at indexLineMax bytes, cutting at a word boundary, rune-safe.
func truncateLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= indexLineMax {
		return s
	}
	cut := strings.ToValidUTF8(s[:indexLineMax], "")
	if i := strings.LastIndexByte(cut, ' '); i > indexLineMax/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// idMarker is a block's persisted ID: assigned once at settle, carried by the block through edits and archive moves.
var idMarker = regexp.MustCompile(`\s*<!-- id:([0-9a-f]{8}) -->`)

// contentRefPrefix leads a derived block ref; 'z' is never hex, so it cannot collide with a real id marker.
const contentRefPrefix = "z"

// derivedRef matches a contentRef: the prefix and a non-empty run of lowercase hex digits.
var derivedRef = regexp.MustCompile("^" + contentRefPrefix + "[0-9a-f]+$")

// contentRef is the stable ref for an unmarked block: the prefix plus 4 bytes of sha256 of its normalized text.
func contentRef(block string) string {
	return contentRefPrefix + textutil.SHA256Hex([]byte(strings.Join(strings.Fields(block), " ")))[:8]
}

// blockRef is the block's persisted id, else fallback, else its contentRef.
func blockRef(block, fallback string) string {
	if id := ownerID(block); id != "" {
		return id
	}
	if fallback != "" {
		return fallback
	}
	return contentRef(block)
}

// idFiles are the typed files whose blocks get IDs, with the prefix that starts a block.
var idFiles = map[string]string{"decisionLog.md": "## ", "sessionHistory.md": "## ", "conventions.md": "- "}

// blockID is the persisted ID on line ("" when it has none).
func blockID(line string) string {
	if m := idMarker.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// ownerID is the id a block answers to: its last unfenced marker, which may be wrapped or stacked below the first line.
func ownerID(block string) string {
	if !strings.Contains(block, "```") && !strings.Contains(block, "~~~") { // no fence: the last marker is the owner
		return lastIDMarker(block)
	}
	lines := strings.Split(block, "\n")
	line, span, total := lastMarker(lines, fencedLines(lines))
	if total == 0 {
		return ""
	}
	return blockID(lines[line][span[0]:span[1]])
}

// lastIDMarker is the id of the last `<!-- id:xxxxxxxx -->` in text, "" when there is none (no regexp scan).
func lastIDMarker(text string) string {
	for end := len(text); ; {
		i := strings.LastIndex(text[:end], "<!-- id:")
		if i < 0 {
			return ""
		}
		if rest := text[i+len("<!-- id:"):]; len(rest) >= 12 && rest[8:12] == " -->" && isLowerHex(rest[:8]) {
			return rest[:8]
		}
		end = i
	}
}

// isLowerHex reports whether s is only 0-9a-f.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// stripID removes the ID marker for display.
func stripID(s string) string { return idMarker.ReplaceAllString(s, "") }

// assignIDs stamps unmarked blocks and collapses stacked markers to the oldest; blocks are judged whole.
func assignIDs(dir string, now time.Time) (int, error) {
	n, _, err := assignIDTexts(dir, now)
	return n, err
}

// assignIDTexts is assignIDs that also returns each present id file's settled text, keyed by file name.
func assignIDTexts(dir string, now time.Time) (int, map[string]string, error) {
	n := 0
	texts := map[string]string{}
	for name, prefix := range idFiles {
		stamped, text, err := assignFileIDs(dir, name, prefix, now)
		n += stamped
		if err != nil {
			return n, texts, err
		}
		if text != "" {
			texts[name] = text
		}
	}
	return n, texts, nil
}

// assignFileIDs is assignIDs for one typed file, returning its settled text; a missing file is skipped ("" text).
func assignFileIDs(dir, name, prefix string, now time.Time) (int, string, error) {
	path := filepath.Join(dir, name)
	data, err := readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	res, n := stampIDs(dir, name, prefix, string(data), now)
	if res != string(data) {
		if err := paths.AtomicWrite(path, []byte(res), 0o600); err != nil {
			return n, res, err
		}
	}
	return n, res, nil
}

// stampIDs is assignFileIDs on text in memory: the settled text and how many blocks it stamped.
func stampIDs(dir, name, prefix, text string, now time.Time) (string, int) {
	n := 0
	lines := strings.Split(text, "\n")
	starts := blockStarts(lines, prefix)
	out := make([]string, 0, len(lines))
	head := len(lines)
	if len(starts) > 0 {
		head = starts[0]
	}
	out = append(out, lines[:head]...)
	for k, i := range starts {
		end := len(lines)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		block, stamped := settleBlockID(dir, name, i, lines[i:end], now)
		for salt := len(lines); stamped == 1 && strings.Contains(text, block[0][len(lines[i])+1:]); salt++ {
			block, _ = settleBlockID(dir, name, salt, lines[i:end], now) // an id text already carries: redraw past its end
		}
		n += stamped
		out = append(out, block...)
	}
	return strings.Join(out, "\n"), n
}

// blockStarts returns the index of each non-empty prefix line outside code fences.
func blockStarts(lines []string, prefix string) []int {
	var starts []int
	fence := ""
	for i, l := range lines {
		if fence == "" && strings.HasPrefix(l, prefix) && strings.TrimSpace(strings.TrimPrefix(l, prefix)) != "" {
			starts = append(starts, i)
		}
		fence = nextFence(fence, l)
	}
	return starts
}

// fencedLines flags each line that is a code fence or sits inside one; markers there are examples, not ids.
func fencedLines(block []string) []bool {
	out := make([]bool, len(block))
	fence := ""
	for i, l := range block {
		next := nextFence(fence, l)
		out[i] = fence != "" || next != ""
		fence = next
	}
	return out
}

// settleBlockID leaves one id marker (the oldest, i.e. the last, else a fresh one); fenced lines are never touched.
func settleBlockID(dir, name string, idx int, block []string, now time.Time) ([]string, int) {
	fenced := fencedLines(block)
	keepLine, keepSpan, total := lastMarker(block, fenced)
	if total == 0 {
		return append([]string{block[0] + freshID(dir, name, idx, block[0], now)}, block[1:]...), 1
	}
	if total == 1 {
		return block, 0
	}
	kept := " " + strings.TrimSpace(block[keepLine][keepSpan[0]:keepSpan[1]])
	out := dropMarkers(block, fenced, keepLine, keepSpan)
	if keepLine != 0 {
		out[0] = strings.TrimRight(out[0], " \t") + kept
	}
	return out, 0
}

// dropMarkers removes every id marker but the survivor; a non-first line left blank by it disappears.
func dropMarkers(block []string, fenced []bool, keepLine int, keepSpan []int) []string {
	out := make([]string, 0, len(block))
	for i, l := range block {
		if !fenced[i] && idMarker.MatchString(l) {
			res := stripMarkers(l, i == 0 && i == keepLine, keepSpan) // a first-line survivor stays where it is
			if strings.TrimSpace(res) == "" && i != 0 {
				continue
			}
			l = res
		}
		out = append(out, l)
	}
	return out
}

// lastMarker finds the last unfenced id marker in block: its line, its span and how many markers there are.
func lastMarker(block []string, fenced []bool) (keepLine int, keepSpan []int, total int) {
	keepLine = -1
	for i, l := range block {
		if fenced[i] {
			continue
		}
		for _, sp := range idMarker.FindAllStringIndex(l, -1) {
			keepLine, keepSpan, total = i, sp, total+1
		}
	}
	return keepLine, keepSpan, total
}

// freshID is the new ` <!-- id:xxxx -->` suffix for a block; idx is in the hash so identical lines never collide.
func freshID(dir, name string, idx int, line string, now time.Time) string {
	key := dir + "\x00" + name + "\x00" + strconv.Itoa(idx) + "\x00" + line + "\x00" + now.Format(time.RFC3339Nano)
	return " <!-- id:" + textutil.SHA256Hex([]byte(key))[:8] + " -->"
}

// stripMarkers removes every id marker from l, except the one at keepSpan when keepHere.
func stripMarkers(l string, keepHere bool, keepSpan []int) string {
	var b strings.Builder
	prev := 0
	for _, sp := range idMarker.FindAllStringIndex(l, -1) {
		b.WriteString(l[prev:sp[0]])
		if keepHere && sp[0] == keepSpan[0] {
			b.WriteString(l[sp[0]:sp[1]])
		}
		prev = sp[1]
	}
	b.WriteString(l[prev:])
	return b.String()
}

// errNoBlock is returned when no block in the store carries the requested ID.
var errNoBlock = errors.New("no memory block with that id")

// ShowBlock finds the block for ref (an id marker or a contentRef) anywhere in the store, archives included.
func ShowBlock(ref string) (string, string, error) {
	derived := derivedRef.MatchString(ref)
	var foundPath, foundBlock string
	err := filepath.WalkDir(StoreDir(), func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) { // no store yet: nothing to find
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		b, ok, err := blockWithRef(p, ref, derived)
		if err != nil {
			return err
		}
		if ok {
			foundPath, foundBlock = p, b
			return filepath.SkipAll // stop at the first file holding it: the rest of the store need not be read
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if foundPath == "" {
		return "", "", errNoBlock
	}
	return foundPath, foundBlock, nil
}

// blockWithRef returns the last block in file p carrying ref (an id marker, or a contentRef when derived).
func blockWithRef(p, ref string, derived bool) (found string, ok bool, err error) {
	data, err := readFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !derived && !strings.Contains(string(data), "<!-- id:"+ref+" -->") {
		return "", false, nil
	}
	_, blocks := entryBlocks(logicalName(p), string(data))
	if derived { // native topics and knowledge notes are indexed whole, under their own grammar
		base := filepath.Base(p)
		blocks = append(blocks, parseBlocks(segNative, base, data)...)
		blocks = append(blocks, parseBlocks(segKnowledge, base, data)...)
	}
	for _, b := range blocks {
		matches := strings.Contains(b, "<!-- id:"+ref+" -->")
		if derived {
			matches = contentRef(b) == ref
		}
		if matches {
			found, ok = b, true
		}
	}
	return found, ok, nil
}

var archiveName = regexp.MustCompile(`^(decisionLog|sessionHistory|conventions)-\d{4}-\d{2}\.md$`)

// logicalName maps an archive file (decisionLog-2026-10.md) back to its typed file's grammar.
func logicalName(path string) string {
	base := filepath.Base(path)
	if _, ok := idFiles[base]; ok {
		return base
	}
	if m := archiveName.FindStringSubmatch(base); m != nil {
		return m[1] + ".md"
	}
	return base
}
